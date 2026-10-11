# lite-docker-proxy 设计文档

日期：2026-10-11
状态：已确认

## 1. 目标

实现一个用 Go 编写的 Docker Registry 镜像服务，将客户端对自身域名（如 `example.com`）的 Registry V2 请求透明转发到一个或多个上游 registry（如 `docker.io`、`ghcr.io`）。

核心诉求：

- 完整的域名镜像：`docker pull example.com/caddy:latest` 无缝映射到 `docker.io/library/caddy:latest`。
- 下游（连接本服务的 Docker daemon）使用用户名 / 密码鉴权。
- 全部配置通过环境变量完成。
- 使用 Gin 框架 + ginlogrus 输出日志。
- 不做镜像缓存，仅按需转发。

## 2. 关键约束

Docker 镜像引用语法的路径段必须以字母数字开头。`example.com/_ghcr/caddy` 会被 Docker 客户端直接拒绝（实测报 `invalid reference format`）。因此上游别名的前缀必须是合法的仓库段，例如 `ghcr`、`docker`，不能使用下划线开头。

## 3. 术语

- **下游**：连接本服务的 Docker daemon / 客户端。
- **上游**：真正提供镜像的 registry，如 `docker.io`、`ghcr.io`。
- **alias**：上游在本服务中的路径前缀，如 `docker`、`ghcr`。
- **repo（repository name）**：Registry V2 路径中 `<name>` 部分，可能包含 `/`。

## 4. 配置（环境变量）

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `LISTEN_ADDR` | 否 | `:8080` | HTTP 监听地址 |
| `PROXY_USERNAME` | 否 | 空 | 下游 Basic 鉴权用户名；为空则关闭下游鉴权 |
| `PROXY_PASSWORD` | 条件 | 空 | 下游 Basic 鉴权密码；设置了用户名时必填 |
| `UPSTREAMS` | 是 | 空 | 上游白名单，格式 `host=alias[,host=alias...]`，如 `docker.io=docker,ghcr.io=ghcr` |
| `DEFAULT_UPSTREAM` | 否 | 空 | 不带 alias 前缀时使用的上游 host，必须存在于 `UPSTREAMS` 中；为空则裸名请求返回 404 |
| `UPSTREAM_CREDENTIALS` | 否 | 空 | 上游凭据，格式 `host=username:password[,...]`，用于拉取私有镜像或更高配额 |
| `LOG_LEVEL` | 否 | `info` | logrus 日志级别（debug/info/warn/error） |
| `UPSTREAM_HEADER_TIMEOUT` | 否 | `60s` | 等待上游响应头的超时（不影响已开始传输的 body） |
| `UPSTREAM_TLS_INSECURE` | 否 | `false` | 是否跳过上游 TLS 校验（调试用） |

校验规则：

- `UPSTREAMS` 为空 → 启动失败。
- alias 必须匹配 `^[a-z0-9][a-z0-9._-]*$`，且全局唯一。
- host 必须唯一。
- `DEFAULT_UPSTREAM` 非空时必须命中 `UPSTREAMS`。
- 设置了 `PROXY_USERNAME` 但未设置 `PROXY_PASSWORD` → 启动失败。

## 5. 路由与映射

### 5.1 端点

| 方法 | 路径 | 行为 |
|---|---|---|
| GET / HEAD | `/v2/` | 鉴权通过后返回 `200`，body `{}`，带 `Docker-Distribution-Api-Version: registry/2.0` |
| GET / HEAD | `/v2/<name>/manifests/<ref>` | 转发 |
| GET / HEAD | `/v2/<name>/blobs/<digest>` | 转发 |
| GET | `/v2/<name>/tags/list` | 转发 |
| GET | `/v2/<name>/referrers/<digest>` | 转发（若上游不支持则透传上游状态码） |
| 其他方法 | `/v2/...` | `405 Method Not Allowed` |
| 其他路径 | — | `404` |

### 5.2 name 解析

对 `/v2/*` 之后的路径，使用贪婪匹配定位最后一个动作关键字，拆出 repo 与剩余部分：

```
^(?P<name>.+)/(?P<kind>manifests|blobs|tags|referrers)(?:/(?P<rest>.*))?$
```

### 5.3 alias 与上游解析

1. 取 repo 的第一段（`/` 之前）。
2. 若该段命中某个 alias：剥离该段，剩余部分作为上游 repo，上游为该 alias 对应的 host。
3. 否则：上游为 `DEFAULT_UPSTREAM`，repo 原样。
4. 若既无 alias 命中且无 `DEFAULT_UPSTREAM`：返回 `404`（`NAME_UNKNOWN`）。

### 5.4 Docker Hub 特例

当解析出的上游 host 属于 Docker Hub（`docker.io` / `index.docker.io` / `registry-1.docker.io`）时：

- 上游请求 host 统一使用 `registry-1.docker.io`。
- 若上游 repo 不包含 `/`，自动补 `library/` 前缀。

其他上游 repo 原样使用。

### 5.5 转发 URL

`https://<upstream-host>/v2/<upstream-repo>/<kind>/<rest>`，保留原始查询字符串（如 `?ns=`）。

## 6. 鉴权

### 6.1 下游鉴权

- 配置了 `PROXY_USERNAME` 时启用。
- 校验请求头 `Authorization: Basic`，使用常量时间比较。
- 失败：`401` + `WWW-Authenticate: Basic realm="lite-docker-proxy"` + Registry 错误 JSON `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`。
- Docker daemon 收到 challenge 后会用 `docker login` 保存的凭据自动重试。
- 未配置用户名时不鉴权（匿名）。

### 6.2 上游鉴权

转发前不预判，采用「先请求、按 challenge 重试」策略：

1. 发送上游请求（可带已缓存的 Authorization）。
2. 若上游返回 `401` 且 `WWW-Authenticate: Bearer realm=...,service=...,scope=...`：
   - 用 `realm` 作为 token endpoint，拼接 `service` 与 `scope`（scope 缺失时用 `repository:<repo>:pull` 构造）。
   - GET token endpoint；若配置了该上游的 `UPSTREAM_CREDENTIALS`，使用 Basic 附带凭据。
   - 解析响应中的 `token` / `access_token` 与 `expires_in`，缓存后重试一次。
3. 若上游返回 `401` 且为 `Basic` challenge，则使用该上游的 `UPSTREAM_CREDENTIALS` 作为 Basic 重试；未配置则把 401 透传给下游。
4. 若上游直接 `200`，直接转发。

Token 缓存：

- 仅缓存 token（按 `host + service + scope` 为 key），不缓存镜像内容。
- 记录过期时间，提前 30 秒视为过期；过期后重新获取。
- 并发安全（`sync.RWMutex`）。

## 7. 转发行为

- 使用 `net/http` 手动转发，而非 `httputil.ReverseProxy`，以便实现 401 重试。
- 请求体：直接使用下游请求的 body（只读场景通常无 body）。
- 响应：复制状态码与端到端响应头，剔除 hop-by-hop 头（`Connection`、`Keep-Alive`、`Transfer-Encoding`、`Upgrade`、`Proxy-*`、`Trailer`、`TE`）。
- body 使用 `io.Copy` 流式写入，不缓冲、不落盘。
- `HEAD` 请求不写 body。
- `3xx` 重定向（Docker Hub blob 的 `307` 到 CDN）**原样透传**，客户端直接向 CDN 拉取，代理不承担镜像流量。
- `Authorization` 请求头不会透传给下游响应。
- 上游不可达 / 超时 → `502` + `{"errors":[{"code":"UNKNOWN","message":"upstream error"}]}`。
- 客户端断开连接时取消上游请求（`c.Request.Context()`）。

## 8. 日志

- 使用 `ginlogrus` 作为 Gin 的 logger 中间件。
- 自定义 formatter，结构化输出字段：`method`、`path`、`status`、`latency`、`client_ip`、`upstream`、`repo`。
- `Authorization` 永不记录；日志中若出现凭据需脱敏。
- 日志级别由 `LOG_LEVEL` 控制，默认 `info`。
- debug 级别记录 alias 解析结果与上游 URL。

## 9. 代码结构

```
main.go                              // 启动、装配
internal/config/config.go            // 环境变量解析与校验
internal/config/config_test.go
internal/registry/router.go          // 路由注册、ping、方法守卫
internal/registry/mapping.go         // alias/repo -> upstream 解析
internal/registry/mapping_test.go
internal/registry/middleware.go      // 下游 Basic 鉴权 + 统一错误响应
internal/registry/proxy.go           // 流式转发
internal/registry/proxy_test.go      // 用 httptest 伪造上游
internal/upstream/auth.go            // Bearer challenge 解析与 token 缓存
internal/upstream/auth_test.go
internal/upstream/client.go          // http.Client / Transport 构造
```

## 10. 错误响应格式

遵循 Registry V2 规范：

```json
{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}
```

使用到的 code：`UNAUTHORIZED`、`NAME_UNKNOWN`、`UNSUPPORTED`、`UNKNOWN`。

## 11. 测试策略

- `config`：解析合法 / 非法环境变量、默认值、校验失败。
- `mapping`：alias 命中 / 未命中、默认上游、Docker Hub `library/` 补齐、多段 repo、无默认上游时 404。
- `upstream/auth`：Bearer challenge 解析、token 缓存命中与过期、Basic challenge。
- `proxy`：通过 `httptest.Server` 模拟上游，验证路径改写、Authorization 注入、401 重试、307 透传、跳过头处理、405/404。
- 尽量不依赖真实网络。

## 12. 非目标（YAGNI）

- 镜像内容缓存 / 本地存储。
- 推送（push）能力。
- Web UI 或管理接口。
- 多用户 / 多角色鉴权（仅一组静态凭据）。
- 指标 / Prometheus 暴露。
- Token 持久化。

## 13. 部署说明

服务本身监听明文 HTTP，生产环境假设由反向代理 / Ingress 终止 TLS（`https://example.com`）。Docker daemon 需要信任对应证书；若使用自签证书需在 daemon 配置 `insecure-registries` 或安装 CA。

使用示例：

```bash
export UPSTREAMS="docker.io=docker,ghcr.io=ghcr"
export DEFAULT_UPSTREAM=docker.io
export PROXY_USERNAME=admin
export PROXY_PASSWORD=secret
export LISTEN_ADDR=:8080
go run .

docker login example.com
docker pull example.com/caddy:latest          # -> docker.io/library/caddy
docker pull example.com/docker/caddy:latest   # -> docker.io/library/caddy
docker pull example.com/docker/user/repo:tag  # -> docker.io/user/repo
docker pull example.com/ghcr/owner/repo:tag   # -> ghcr.io/owner/repo
```
