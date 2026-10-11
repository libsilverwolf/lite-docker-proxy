# lite-docker-proxy

A lightweight Docker Registry V2 pull-through proxy. It maps requests on your
own domain to one or more whitelisted upstream registries, with downstream
Basic auth. It does **not** cache images.

## Configuration

| Env | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address (TLS is terminated externally) |
| `PROXY_USERNAME` | – | Downstream Basic auth username; empty disables auth |
| `PROXY_PASSWORD` | – | Downstream Basic auth password |
| `UPSTREAMS` | – | Whitelist, `host=alias[,host=alias]`, e.g. `docker.io=docker,ghcr.io=ghcr` |
| `DEFAULT_UPSTREAM` | – | Upstream host used when no alias prefix is present (must be whitelisted) |
| `UPSTREAM_CREDENTIALS` | – | Optional, `host=user:pass[,host=user:pass]` |
| `LOG_LEVEL` | `info` | debug / info / warn / error |
| `UPSTREAM_HEADER_TIMEOUT` | `60s` | Upstream response-header timeout |
| `UPSTREAM_TLS_INSECURE` | `false` | Skip upstream TLS verification (debug only) |

An upstream host may include a scheme for internal registries, e.g.
`UPSTREAMS="http://registry.internal:5000=internal"`.

## Usage

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

Requests go to plain HTTP on `LISTEN_ADDR`; put a TLS-terminating reverse proxy
or ingress in front for production.

## Docker

```bash
docker build -t lite-docker-proxy:latest .
docker run -d --name lite-docker-proxy -p 8080:8080 \
  -e UPSTREAMS="docker.io=docker,ghcr.io=ghcr" \
  -e DEFAULT_UPSTREAM=docker.io \
  -e PROXY_USERNAME=admin \
  -e PROXY_PASSWORD=secret \
  lite-docker-proxy:latest
```

The image runs as an unprivileged user and installs `ca-certificates` (required
for HTTPS to the upstream registries). The `HEALTHCHECK` calls the built-in
`lite-docker-proxy healthcheck` subcommand, which probes the unauthenticated
`GET /healthz` endpoint on `LISTEN_ADDR`. Do not point the health check at
`/v2/`: it returns `401` when downstream auth is enabled.

## Behavior

- Only `GET` and `HEAD` are allowed; pushes return `405`.
- Upstream Bearer challenges are resolved automatically and tokens are cached
  in memory until shortly before expiry. Blob `307` redirects are passed
  through to the client.
- Docker Hub single-segment names are expanded with the `library/` namespace and
  requests use `registry-1.docker.io`.

## LICENSE

MIT