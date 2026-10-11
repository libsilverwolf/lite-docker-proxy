# lite-docker-proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go/Gin HTTP Docker Registry proxy that mirrors pull requests on `example.com` to whitelisted upstream registries, with downstream Basic auth and no image caching.

**Architecture:** Inbound plain-HTTP Gin server (TLS terminated externally). A catch-all `/v2/*path` handler parses the registry route, resolves a whitelisted upstream from the first repository path segment (alias) or the configured default, obtains an upstream Bearer token on demand, and streams the upstream response back with `io.Copy`. Tokens are cached in memory; image data is never cached.

**Tech Stack:** Go 1.25+, Gin, `github.com/toorop/gin-logrus`, `github.com/sirupsen/logrus`, standard `net/http`.

---

## File Structure

- `internal/config/config.go` — env parsing/validation, `Upstream`/`Config` types.
- `internal/config/config_test.go`
- `internal/registry/mapping.go` — registry route parsing + alias/default → upstream resolution.
- `internal/registry/mapping_test.go`
- `internal/registry/errors.go` — Registry V2 error JSON helper.
- `internal/registry/middleware.go` — downstream Basic auth.
- `internal/registry/proxy.go` — streaming forwarder with 401 retry.
- `internal/registry/proxy_test.go`
- `internal/registry/router.go` — routes, ping, method guard.
- `internal/upstream/client.go` — `http.Client`/`Transport` construction.
- `internal/upstream/auth.go` — challenge parsing, token fetch/cache.
- `internal/upstream/auth_test.go`
- `main.go` — assembly + graceful shutdown.
- `README.md` — usage.

---

### Task 1: Config package

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"testing"
	"time"
)

func mapGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestLoadValid(t *testing.T) {
	env := map[string]string{
		"UPSTREAMS":        "docker.io=docker,ghcr.io=ghcr",
		"DEFAULT_UPSTREAM": "docker.io",
		"PROXY_USERNAME":   "admin",
		"PROXY_PASSWORD":   "secret",
	}
	cfg, err := load(mapGetenv(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.UpstreamHeaderTimeout != 60*time.Second {
		t.Errorf("timeout = %v", cfg.UpstreamHeaderTimeout)
	}
	u, ok := cfg.UpstreamByAlias("docker")
	if !ok || u.Host != "docker.io" || u.Scheme != "https" {
		t.Fatalf("docker alias = %+v, ok=%v", u, ok)
	}
	if u.RequestHost() != "registry-1.docker.io" {
		t.Errorf("RequestHost = %q", u.RequestHost())
	}
	if cfg.Username != "admin" || cfg.Password != "secret" {
		t.Errorf("creds = %q/%q", cfg.Username, cfg.Password)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"missing upstreams", map[string]string{}},
		{"duplicate alias", map[string]string{"UPSTREAMS": "docker.io=docker,ghcr.io=docker"}},
		{"duplicate host", map[string]string{"UPSTREAMS": "docker.io=docker,docker.io=d"}},
		{"default not in list", map[string]string{"UPSTREAMS": "docker.io=docker", "DEFAULT_UPSTREAM": "ghcr.io"}},
		{"bad alias", map[string]string{"UPSTREAMS": "docker.io=_docker"}},
		{"user without pass", map[string]string{"UPSTREAMS": "docker.io=docker", "PROXY_USERNAME": "admin"}},
		{"bad timeout", map[string]string{"UPSTREAMS": "docker.io=docker", "UPSTREAM_HEADER_TIMEOUT": "nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := load(mapGetenv(tc.env)); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestUpstreamCredentials(t *testing.T) {
	env := map[string]string{
		"UPSTREAMS":            "ghcr.io=ghcr",
		"UPSTREAM_CREDENTIALS": "ghcr.io=user:pa:ss",
	}
	cfg, err := load(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := cfg.UpstreamByHost("ghcr.io")
	if u.Username != "user" || u.Password != "pa:ss" {
		t.Fatalf("creds = %q/%q", u.Username, u.Password)
	}
}

func TestUpstreamSchemeInHost(t *testing.T) {
	env := map[string]string{"UPSTREAMS": "http://registry.internal:5000=internal"}
	cfg, err := load(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	u, ok := cfg.UpstreamByAlias("internal")
	if !ok || u.Host != "registry.internal:5000" || u.Scheme != "http" {
		t.Fatalf("upstream = %+v", u)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL (package/files missing, `load` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/config/config.go`:

```go
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr       = ":8080"
	defaultLogLevel         = "info"
	defaultHeaderTimeout    = 60 * time.Second
	defaultScheme           = "https"
)

var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Upstream is a whitelisted upstream registry reachable through an alias prefix.
type Upstream struct {
	Host     string // hostname[:port], lowercase, no scheme
	Scheme   string // "https" (default) or "http"
	Alias    string // path prefix, e.g. "docker"
	Username string
	Password string
}

// IsDockerHub reports whether this upstream is Docker Hub.
func (u Upstream) IsDockerHub() bool {
	switch u.Host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	}
	return false
}

// RequestHost returns the host to use for upstream HTTP requests.
func (u Upstream) RequestHost() string {
	if u.IsDockerHub() {
		return "registry-1.docker.io"
	}
	return u.Host
}

// BaseURL returns the upstream base URL.
func (u Upstream) BaseURL() *url.URL {
	scheme := u.Scheme
	if scheme == "" {
		scheme = defaultScheme
	}
	return &url.URL{Scheme: scheme, Host: u.RequestHost()}
}

// Config is the fully validated runtime configuration.
type Config struct {
	ListenAddr            string
	Username              string
	Password              string
	DefaultUpstream       string // host of the default upstream, may be empty
	Upstreams             []Upstream
	LogLevel              string
	UpstreamHeaderTimeout time.Duration
	UpstreamTLSInsecure   bool
}

func (c *Config) UpstreamByAlias(alias string) (Upstream, bool) {
	for _, u := range c.Upstreams {
		if u.Alias == alias {
			return u, true
		}
	}
	return Upstream{}, false
}

func (c *Config) UpstreamByHost(host string) (Upstream, bool) {
	for _, u := range c.Upstreams {
		if u.Host == host {
			return u, true
		}
	}
	return Upstream{}, false
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (*Config, error) {
	c := &Config{
		ListenAddr: firstNonEmpty(getenv("LISTEN_ADDR"), defaultListenAddr),
		Username:   strings.TrimSpace(getenv("PROXY_USERNAME")),
		Password:   getenv("PROXY_PASSWORD"),
		LogLevel:   firstNonEmpty(getenv("LOG_LEVEL"), defaultLogLevel),
	}

	upstreams, err := parseUpstreams(getenv("UPSTREAMS"))
	if err != nil {
		return nil, err
	}
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("UPSTREAMS must not be empty")
	}

	creds, err := parseCredentials(getenv("UPSTREAM_CREDENTIALS"))
	if err != nil {
		return nil, err
	}
	for i := range upstreams {
		if cred, ok := creds[upstreams[i].Host]; ok {
			upstreams[i].Username = cred[0]
			upstreams[i].Password = cred[1]
		}
	}
	c.Upstreams = upstreams

	defaultHost := strings.ToLower(strings.TrimSpace(getenv("DEFAULT_UPSTREAM")))
	if defaultHost != "" {
		if _, ok := c.UpstreamByHost(defaultHost); !ok {
			return nil, fmt.Errorf("DEFAULT_UPSTREAM %q is not in UPSTREAMS", defaultHost)
		}
	}
	c.DefaultUpstream = defaultHost

	if c.Username != "" && c.Password == "" {
		return nil, fmt.Errorf("PROXY_PASSWORD must be set when PROXY_USERNAME is set")
	}

	if v := strings.TrimSpace(getenv("UPSTREAM_HEADER_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid UPSTREAM_HEADER_TIMEOUT %q", v)
		}
		c.UpstreamHeaderTimeout = d
	} else {
		c.UpstreamHeaderTimeout = defaultHeaderTimeout
	}

	if v := strings.TrimSpace(getenv("UPSTREAM_TLS_INSECURE")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid UPSTREAM_TLS_INSECURE %q", v)
		}
		c.UpstreamTLSInsecure = b
	}

	return c, nil
}

func parseUpstreams(raw string) ([]Upstream, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seenHost := map[string]bool{}
	seenAlias := map[string]bool{}
	var out []Upstream
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid UPSTREAMS entry %q, want host=alias", part)
		}
		host, scheme, err := normalizeHost(kv[0])
		if err != nil {
			return nil, fmt.Errorf("invalid UPSTREAMS entry %q: %w", part, err)
		}
		alias := strings.ToLower(strings.TrimSpace(kv[1]))
		if alias == "" {
			return nil, fmt.Errorf("invalid UPSTREAMS entry %q, alias is required", part)
		}
		if !aliasRe.MatchString(alias) {
			return nil, fmt.Errorf("invalid alias %q: must match %s", alias, aliasRe.String())
		}
		if seenHost[host] {
			return nil, fmt.Errorf("duplicate upstream host %q", host)
		}
		if seenAlias[alias] {
			return nil, fmt.Errorf("duplicate upstream alias %q", alias)
		}
		seenHost[host] = true
		seenAlias[alias] = true
		out = append(out, Upstream{Host: host, Scheme: scheme, Alias: alias})
	}
	return out, nil
}

func normalizeHost(raw string) (host, scheme string, err error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	scheme = defaultScheme
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return "", "", fmt.Errorf("invalid upstream host %q", raw)
		}
		scheme = parsed.Scheme
		host = parsed.Host
	} else {
		host = raw
	}
	if host == "" {
		return "", "", fmt.Errorf("empty upstream host")
	}
	return host, scheme, nil
}

func parseCredentials(raw string) (map[string][2]string, error) {
	out := map[string][2]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid UPSTREAM_CREDENTIALS entry %q, want host=user:pass", part)
		}
		host := strings.ToLower(strings.TrimSpace(kv[0]))
		userpass := strings.SplitN(kv[1], ":", 2)
		if host == "" || len(userpass) != 2 || userpass[0] == "" {
			return nil, fmt.Errorf("invalid UPSTREAM_CREDENTIALS entry %q, want host=user:pass", part)
		}
		out[host] = [2]string{userpass[0], userpass[1]}
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: add env-based configuration for upstreams and auth"
```

---

### Task 2: Route parsing and upstream mapping

**Files:**
- Create: `internal/registry/mapping.go`
- Test: `internal/registry/mapping_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/registry/mapping_test.go`:

```go
package registry

import (
	"testing"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		DefaultUpstream: "docker.io",
		Upstreams: []config.Upstream{
			{Host: "docker.io", Scheme: "https", Alias: "docker"},
			{Host: "ghcr.io", Scheme: "https", Alias: "ghcr"},
		},
	}
}

func TestParseRoute(t *testing.T) {
	r, err := ParseRoute("/caddy/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "caddy" || r.Kind != "manifests" || r.Rest != "latest" {
		t.Fatalf("route = %+v", r)
	}

	r, err = ParseRoute("/foo/bar/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "foo/bar" || r.Kind != "blobs" || r.Rest != "sha256:abc" {
		t.Fatalf("route = %+v", r)
	}

	if _, err := ParseRoute("/foo"); err == nil {
		t.Fatal("expected error for /foo")
	}
}

func TestResolveTarget(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		name     string
		wantHost string
		wantRepo string
		wantOK   bool
	}{
		{"caddy", "docker.io", "library/caddy", true},
		{"docker/caddy", "docker.io", "library/caddy", true},
		{"docker/user/repo", "docker.io", "user/repo", true},
		{"ghcr/owner/repo", "ghcr.io", "owner/repo", true},
		{"evil.example/repo", "docker.io", "evil.example/repo", true},
		{"ghcr", "", "", false},
	}
	for _, tc := range cases {
		target, ok := ResolveTarget(cfg, tc.name)
		if ok != tc.wantOK {
			t.Errorf("%s: ok=%v want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if target.Upstream.Host != tc.wantHost || target.Repo != tc.wantRepo {
			t.Errorf("%s: got host=%q repo=%q", tc.name, target.Upstream.Host, target.Repo)
		}
	}
}

func TestResolveTargetNoDefault(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultUpstream = ""
	if _, ok := ResolveTarget(cfg, "caddy"); ok {
		t.Fatal("expected no target without default upstream")
	}
	if _, ok := ResolveTarget(cfg, "ghcr/owner/repo"); !ok {
		t.Fatal("alias should still resolve")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/registry/`
Expected: FAIL (`ParseRoute`/`ResolveTarget` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/registry/mapping.go`:

```go
package registry

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

var routeRe = regexp.MustCompile(`^(?P<name>.+)/(?P<kind>manifests|blobs|tags|referrers)(?:/(?P<rest>.*))?$`)

// Route is a parsed Registry V2 request path (without the /v2 prefix).
type Route struct {
	Name string // raw repository name, including any alias prefix
	Kind string // manifests | blobs | tags | referrers
	Rest string // reference, digest, upload id, or "list"
}

// ParseRoute parses the path following "/v2".
func ParseRoute(path string) (Route, error) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return Route{}, fmt.Errorf("empty registry path")
	}
	m := routeRe.FindStringSubmatch(path)
	if m == nil {
		return Route{}, fmt.Errorf("unsupported registry path %q", path)
	}
	return Route{Name: m[1], Kind: m[2], Rest: m[3]}, nil
}

// Target is a resolved upstream request target.
type Target struct {
	Upstream config.Upstream
	Repo     string
	BaseURL  *url.URL
}

// ResolveTarget maps a raw repository name to a whitelisted upstream.
func ResolveTarget(cfg *config.Config, name string) (Target, bool) {
	first, rest, hasSlash := strings.Cut(name, "/")
	if u, ok := cfg.UpstreamByAlias(first); ok {
		if !hasSlash || rest == "" {
			return Target{}, false
		}
		return newTarget(u, dockerHubRepo(u, rest)), true
	}
	if cfg.DefaultUpstream == "" {
		return Target{}, false
	}
	u, ok := cfg.UpstreamByHost(cfg.DefaultUpstream)
	if !ok {
		return Target{}, false
	}
	return newTarget(u, dockerHubRepo(u, name)), true
}

func newTarget(u config.Upstream, repo string) Target {
	return Target{Upstream: u, Repo: repo, BaseURL: u.BaseURL()}
}

// dockerHubRepo adds the "library/" namespace for single-segment Docker Hub names.
func dockerHubRepo(u config.Upstream, repo string) string {
	if u.IsDockerHub() && !strings.Contains(repo, "/") {
		return "library/" + repo
	}
	return repo
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/registry/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry
git commit -m "feat: parse registry routes and resolve whitelisted upstreams"
```

---

### Task 3: Upstream HTTP client

**Files:**
- Create: `internal/upstream/client.go`

- [ ] **Step 1: Write the implementation**

Create `internal/upstream/client.go`:

```go
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

// NewClient builds an HTTP client tuned for streaming registry traffic.
// Redirects are returned to the caller (blob 307s are passed through).
func NewClient(cfg *config.Config) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.UpstreamHeaderTimeout,
		DisableCompression:    true,
	}
	if cfg.UpstreamTLSInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for self-signed upstreams
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/upstream/`
Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/upstream/client.go
git commit -m "feat: add streaming-friendly upstream HTTP client"
```

---

### Task 4: Upstream authentication (challenge, token fetch, cache)

**Files:**
- Create: `internal/upstream/auth.go`
- Test: `internal/upstream/auth_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/upstream/auth_test.go`:

```go
package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

func TestParseChallengeBearer(t *testing.T) {
	ch, err := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/caddy:pull"`)
	if err != nil {
		t.Fatal(err)
	}
	if ch.scheme != "bearer" || ch.realm != "https://auth.docker.io/token" ||
		ch.service != "registry.docker.io" || ch.scope != "repository:library/caddy:pull" {
		t.Fatalf("challenge = %+v", ch)
	}
}

func TestParseChallengeBasic(t *testing.T) {
	ch, err := parseChallenge(`Basic realm="registry"`)
	if err != nil {
		t.Fatal(err)
	}
	if ch.scheme != "basic" {
		t.Fatalf("scheme = %q", ch.scheme)
	}
}

func TestTokenCacheExpiry(t *testing.T) {
	c := newTokenCache()
	c.set("k", "v", 5*time.Minute)
	if _, ok := c.get("k"); !ok {
		t.Fatal("expected cache hit")
	}
	c.mu.Lock()
	c.tokens["expired"] = cachedToken{value: "v", expiresAt: time.Now().Add(-time.Second)}
	c.mu.Unlock()
	if _, ok := c.get("expired"); ok {
		t.Fatal("expected cache miss")
	}
}

func TestHandleChallengeFetchesAndCachesToken(t *testing.T) {
	var gotService, gotScope string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotService = r.URL.Query().Get("service")
		gotScope = r.URL.Query().Get("scope")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"abc","expires_in":300}`))
	}))
	defer tokenServer.Close()

	auth := NewAuthenticator(tokenServer.Client())
	u := config.Upstream{Host: "example.com", Scheme: "https", Alias: "ex"}
	header, err := auth.HandleChallenge(context.Background(), u, "library/caddy",
		`Bearer realm="`+tokenServer.URL+`",service="registry.example.com"`)
	if err != nil {
		t.Fatal(err)
	}
	if header != "Bearer abc" {
		t.Fatalf("header = %q", header)
	}
	if gotService != "registry.example.com" || gotScope != "repository:library/caddy:pull" {
		t.Fatalf("service=%q scope=%q", gotService, gotScope)
	}
	if cached := auth.Authorize(context.Background(), u, "library/caddy"); cached != "Bearer abc" {
		t.Fatalf("cached = %q", cached)
	}
}

func TestHandleChallengeBasicRequiresCredentials(t *testing.T) {
	auth := NewAuthenticator(http.DefaultClient)
	u := config.Upstream{Host: "example.com"}
	if _, err := auth.HandleChallenge(context.Background(), u, "repo", `Basic realm="x"`); err == nil {
		t.Fatal("expected error without configured credentials")
	}
	u.Username, u.Password = "u", "p"
	header, err := auth.HandleChallenge(context.Background(), u, "repo", `Basic realm="x"`)
	if err != nil {
		t.Fatal(err)
	}
	if header != "Basic dTpw" {
		t.Fatalf("header = %q", header)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/upstream/`
Expected: FAIL (`parseChallenge`, `newTokenCache`, `NewAuthenticator` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/upstream/auth.go`:

```go
package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

const (
	tokenSafetyWindow = 30 * time.Second
	maxTokenBodyBytes = 1 << 20
)

type challenge struct {
	scheme  string // "bearer" or "basic"
	realm   string
	service string
	scope   string
}

func parseChallenge(header string) (challenge, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return challenge{}, fmt.Errorf("empty WWW-Authenticate header")
	}
	scheme, params, _ := strings.Cut(header, " ")
	ch := challenge{scheme: strings.ToLower(strings.TrimSpace(scheme))}
	if ch.scheme != "bearer" {
		return ch, nil
	}
	for _, kv := range splitParams(params) {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			ch.realm = value
		case "service":
			ch.service = value
		case "scope":
			ch.scope = value
		}
	}
	return ch, nil
}

// splitParams splits a comma-separated auth parameter list, ignoring commas in quotes.
func splitParams(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if strings.TrimSpace(b.String()) != "" {
		out = append(out, strings.TrimSpace(b.String()))
	}
	return out
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

type tokenCache struct {
	mu     sync.RWMutex
	tokens map[string]cachedToken
}

func newTokenCache() *tokenCache {
	return &tokenCache{tokens: map[string]cachedToken{}}
}

func (c *tokenCache) get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tok, ok := c.tokens[key]
	if !ok || time.Now().After(tok.expiresAt) {
		return "", false
	}
	return tok.value, true
}

func (c *tokenCache) set(key, value string, ttl time.Duration) {
	effective := ttl - tokenSafetyWindow
	if effective <= 0 {
		effective = ttl / 2
	}
	if effective <= 0 {
		effective = time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[key] = cachedToken{value: value, expiresAt: time.Now().Add(effective)}
}

// Authenticator obtains and caches upstream bearer tokens.
type Authenticator struct {
	client *http.Client
	cache  *tokenCache
}

func NewAuthenticator(client *http.Client) *Authenticator {
	return &Authenticator{client: client, cache: newTokenCache()}
}

func pullScope(repo string) string {
	return "repository:" + repo + ":pull"
}

// Authorize returns a cached Authorization header, or "" when no token is cached yet.
func (a *Authenticator) Authorize(_ context.Context, u config.Upstream, repo string) string {
	if tok, ok := a.cache.get(cacheKey(u, pullScope(repo))); ok {
		return "Bearer " + tok
	}
	return ""
}

// HandleChallenge resolves a 401 WWW-Authenticate challenge into an Authorization header.
func (a *Authenticator) HandleChallenge(ctx context.Context, u config.Upstream, repo, header string) (string, error) {
	ch, err := parseChallenge(header)
	if err != nil {
		return "", err
	}
	switch ch.scheme {
	case "basic":
		if u.Username == "" {
			return "", fmt.Errorf("upstream %s requires basic auth but no credentials are configured", u.Host)
		}
		token := base64.StdEncoding.EncodeToString([]byte(u.Username + ":" + u.Password))
		return "Basic " + token, nil
	case "bearer":
		scope := ch.scope
		if scope == "" {
			scope = pullScope(repo)
		}
		token, ttl, err := a.fetchToken(ctx, u, ch.realm, ch.service, scope)
		if err != nil {
			return "", err
		}
		a.cache.set(cacheKey(u, scope), token, ttl)
		return "Bearer " + token, nil
	default:
		return "", fmt.Errorf("unsupported auth scheme %q", ch.scheme)
	}
}

func cacheKey(u config.Upstream, scope string) string {
	return u.Host + "|" + scope
}

func (a *Authenticator) fetchToken(ctx context.Context, u config.Upstream, realm, service, scope string) (string, time.Duration, error) {
	if realm == "" {
		return "", 0, fmt.Errorf("upstream challenge is missing a realm")
	}
	endpoint, err := url.Parse(realm)
	if err != nil {
		return "", 0, fmt.Errorf("invalid token realm %q: %w", realm, err)
	}
	q := endpoint.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	if u.Username != "" {
		req.SetBasicAuth(u.Username, u.Password)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", 0, fmt.Errorf("invalid token response: %w", err)
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", 0, fmt.Errorf("token endpoint returned an empty token")
	}
	ttl := time.Duration(payload.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	return token, ttl, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/upstream/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/upstream/auth.go internal/upstream/auth_test.go
git commit -m "feat: add upstream bearer challenge handling with token cache"
```

---

### Task 5: Error responses and downstream auth middleware

**Files:**
- Create: `internal/registry/errors.go`
- Create: `internal/registry/middleware.go`

- [ ] **Step 1: Write the implementation**

Create `internal/registry/errors.go`:

```go
package registry

import "github.com/gin-gonic/gin"

type errorBody struct {
	Errors []errorEntry `json:"errors"`
}

type errorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes a Registry V2 error response.
func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, errorBody{Errors: []errorEntry{{Code: code, Message: message}}})
}
```

Create `internal/registry/middleware.go`:

```go
package registry

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

const basicRealm = `Basic realm="lite-docker-proxy"`

// AuthMiddleware enforces downstream Basic auth when username is non-empty.
func AuthMiddleware(username, password string) gin.HandlerFunc {
	if username == "" {
		return func(c *gin.Context) { c.Next() }
	}
	expectedUser := []byte(username)
	expectedPass := []byte(password)
	return func(c *gin.Context) {
		user, pass, ok := c.Request.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), expectedUser) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), expectedPass) != 1 {
			c.Header("WWW-Authenticate", basicRealm)
			writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			c.Abort()
			return
		}
		c.Next()
	}
}
```

- [ ] **Step 2: Verify it compiles**

Run: `go build ./internal/registry/`
Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/registry/errors.go internal/registry/middleware.go
git commit -m "feat: add registry error responses and downstream basic auth"
```

---

### Task 6: Streaming proxy forwarder

**Files:**
- Create: `internal/registry/proxy.go`
- Test: `internal/registry/proxy_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/registry/proxy_test.go`:

```go
package registry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
	"github.com/libsilverwolf/lite-docker-proxy/internal/upstream"
)

func newProxyFor(srv *httptest.Server) *Proxy {
	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return NewProxy(&config.Config{}, client, upstream.NewAuthenticator(client), logrus.New())
}

func targetFor(srv *httptest.Server) Target {
	u, _ := url.Parse(srv.URL)
	return Target{
		Upstream: config.Upstream{Host: u.Host, Scheme: u.Scheme, Alias: "test"},
		Repo:     "library/caddy",
		BaseURL:  u,
	}
}

func newGinContext(method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, nil)
	return c, rec
}

func TestForwardRewritesPathAndStripsDownstreamAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = io.WriteString(w, "manifest-body")
	}))
	defer srv.Close()

	p := newProxyFor(srv)
	c, rec := newGinContext(http.MethodGet, "/v2/ghcr/owner/repo/manifests/latest?ns=foo")
	c.Request.Header.Set("Authorization", "Basic Zm9vOmJhcg==")

	target := targetFor(srv)
	target.Repo = "owner/repo"
	p.Forward(c, Route{Name: "ghcr/owner/repo", Kind: "manifests", Rest: "latest"}, target)

	if gotPath != "/v2/owner/repo/manifests/latest" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotQuery != "ns=foo" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
	if gotAuth != "" {
		t.Fatalf("downstream Authorization leaked upstream: %q", gotAuth)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "manifest-body" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Docker-Content-Digest") != "sha256:deadbeef" {
		t.Fatalf("digest header not forwarded")
	}
	if rec.Header().Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Fatalf("api version header = %q", rec.Header().Get("Docker-Distribution-Api-Version"))
	}
}

func TestForwardPassesRedirectThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://cdn.example.com/blob")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	p := newProxyFor(srv)
	c, rec := newGinContext(http.MethodGet, "/v2/test/blobs/sha256:abc")
	p.Forward(c, Route{Name: "test/blobs", Kind: "blobs", Rest: "sha256:abc"}, targetFor(srv))

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Location") != "https://cdn.example.com/blob" {
		t.Fatalf("location = %q", rec.Header().Get("Location"))
	}
}

func TestForwardRetriesWithBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var realm string
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"tok","expires_in":300}`)
			return
		}
		upstreamCalls++
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="registry.test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "manifest")
	}))
	defer srv.Close()
	realm = srv.URL + "/token"

	p := newProxyFor(srv)
	c, rec := newGinContext(http.MethodGet, "/v2/test/manifests/latest")
	p.Forward(c, Route{Name: "test/manifests", Kind: "manifests", Rest: "latest"}, targetFor(srv))

	if rec.Code != http.StatusOK || rec.Body.String() != "manifest" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d want 2", upstreamCalls)
	}
}

func TestForwardUnreachableUpstreamReturnsBadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	urlCopy, _ := url.Parse(srv.URL)
	srv.Close() // now unreachable

	p := newProxyFor(srv)
	c, rec := newGinContext(http.MethodGet, "/v2/test/manifests/latest")
	target := Target{
		Upstream: config.Upstream{Host: urlCopy.Host, Scheme: urlCopy.Scheme, Alias: "test"},
		Repo:     "library/caddy",
		BaseURL:  urlCopy,
	}
	p.Forward(c, Route{Name: "test/manifests", Kind: "manifests", Rest: "latest"}, target)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d want 502", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/registry/`
Expected: FAIL (`NewProxy`/`Forward` undefined).

- [ ] **Step 3: Write the implementation**

Create `internal/registry/proxy.go`:

```go
package registry

import (
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
	"github.com/libsilverwolf/lite-docker-proxy/internal/upstream"
)

const apiVersion = "registry/2.0"

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// Proxy forwards Registry V2 requests to upstream registries.
type Proxy struct {
	cfg    *config.Config
	client *http.Client
	auth   *upstream.Authenticator
	logger *logrus.Logger
}

func NewProxy(cfg *config.Config, client *http.Client, auth *upstream.Authenticator, logger *logrus.Logger) *Proxy {
	if logger == nil {
		logger = logrus.New()
	}
	return &Proxy{cfg: cfg, client: client, auth: auth, logger: logger}
}

// Forward proxies a single request and streams the response back.
func (p *Proxy) Forward(c *gin.Context, route Route, target Target) {
	ctx := c.Request.Context()
	upstreamURL, err := buildUpstreamURL(target, route, c.Request.URL.RawQuery)
	if err != nil {
		writeError(c, http.StatusBadGateway, "UNKNOWN", "invalid upstream request")
		return
	}

	p.logger.WithFields(logrus.Fields{
		"upstream": target.Upstream.Host,
		"repo":     target.Repo,
		"method":   c.Request.Method,
		"url":      upstreamURL,
	}).Debug("forwarding registry request")

	authHeader := p.auth.Authorize(ctx, target.Upstream, target.Repo)
	resp, err := p.do(c, upstreamURL, authHeader)
	if err != nil {
		p.logUpstreamError(upstreamURL, err)
		writeError(c, http.StatusBadGateway, "UNKNOWN", "upstream request failed")
		return
	}

	if resp.StatusCode == http.StatusUnauthorized {
		retryHeader, ok := p.refreshAuth(c, target, resp.Header.Get("WWW-Authenticate"))
		if !ok {
			resp.Body.Close()
			writeError(c, http.StatusBadGateway, "UNKNOWN", "upstream authentication failed")
			return
		}
		resp.Body.Close()
		resp, err = p.do(c, upstreamURL, retryHeader)
		if err != nil {
			p.logUpstreamError(upstreamURL, err)
			writeError(c, http.StatusBadGateway, "UNKNOWN", "upstream request failed")
			return
		}
	}
	defer resp.Body.Close()

	copyResponse(c, resp)
}

func (p *Proxy) refreshAuth(c *gin.Context, target Target, challengeHeader string) (string, bool) {
	if challengeHeader == "" {
		return "", false
	}
	header, err := p.auth.HandleChallenge(c.Request.Context(), target.Upstream, target.Repo, challengeHeader)
	if err != nil {
		p.logger.WithError(err).WithField("upstream", target.Upstream.Host).Warn("upstream authentication failed")
		return "", false
	}
	return header, true
}

func (p *Proxy) do(c *gin.Context, targetURL, authHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, c.Request.Body)
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, c.Request.Header)
	req.Header.Del("Authorization")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Host = req.URL.Host
	return p.client.Do(req)
}

func (p *Proxy) logUpstreamError(upstreamURL string, err error) {
	p.logger.WithError(err).WithField("url", upstreamURL).Error("upstream request failed")
}

func buildUpstreamURL(target Target, route Route, rawQuery string) (string, error) {
	if target.BaseURL == nil {
		return "", &url.Error{Op: "build", URL: "", Err: errNilBaseURL}
	}
	u := *target.BaseURL
	u.Path = "/v2/" + target.Repo + "/" + route.Kind
	if route.Rest != "" {
		u.Path += "/" + route.Rest
	}
	u.RawQuery = rawQuery
	return u.String(), nil
}

func copyHeaders(dst, src http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
	for _, name := range hopHeaders {
		dst.Del(name)
	}
}

func copyResponse(c *gin.Context, resp *http.Response) {
	for _, name := range hopHeaders {
		resp.Header.Del(name)
	}
	for k, values := range resp.Header {
		for _, v := range values {
			c.Writer.Header().Add(k, v)
		}
	}
	if c.Writer.Header().Get("Docker-Distribution-Api-Version") == "" {
		c.Writer.Header().Set("Docker-Distribution-Api-Version", apiVersion)
	}
	c.Status(resp.StatusCode)
	if c.Request.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		_ = c.Error(err)
	}
}
```

Also add `errNilBaseURL` to `mapping.go` or a new small var. Add to `proxy.go` requires `errors`:

Add at top of proxy.go:

```go
var errNilBaseURL = errors.New("target base URL is nil")
```

and import `errors`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/registry/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/proxy.go internal/registry/proxy_test.go
git commit -m "feat: add streaming registry proxy with upstream 401 retry"
```

---

### Task 7: Router and main wiring

**Files:**
- Create: `internal/registry/router.go`
- Create: `main.go`

- [ ] **Step 1: Write the implementation**

Create `internal/registry/router.go`:

```go
package registry

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	ginlogrus "github.com/toorop/gin-logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

// NewRouter builds the Gin engine with logging, recovery and registry routes.
func NewRouter(cfg *config.Config, proxy *Proxy, logger *logrus.Logger) *gin.Engine {
	r := gin.New()
	r.Use(ginlogrus.Logger(logger))
	r.Use(gin.Recovery())

	auth := AuthMiddleware(cfg.Username, cfg.Password)
	r.Any("/v2", auth, pingHandler)
	r.Any("/v2/*path", auth, dispatchHandler(cfg, proxy))
	return r
}

func pingHandler(c *gin.Context) {
	c.Header("Docker-Distribution-Api-Version", apiVersion)
	c.JSON(http.StatusOK, gin.H{})
}

func dispatchHandler(cfg *config.Config, proxy *Proxy) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Param("path")
		if p == "" || p == "/" {
			pingHandler(c)
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Header("Allow", "GET, HEAD")
			writeError(c, http.StatusMethodNotAllowed, "UNSUPPORTED", "read-only proxy: pushes are not supported")
			return
		}
		route, err := ParseRoute(p)
		if err != nil {
			writeError(c, http.StatusNotFound, "NAME_UNKNOWN", err.Error())
			return
		}
		target, ok := ResolveTarget(cfg, route.Name)
		if !ok {
			writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "repository not found: no matching upstream alias and no default upstream configured")
			return
		}
		proxy.Forward(c, route, target)
	}
}
```

Create `main.go`:

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
	"github.com/libsilverwolf/lite-docker-proxy/internal/registry"
	"github.com/libsilverwolf/lite-docker-proxy/internal/upstream"
)

func main() {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	cfg, err := config.Load()
	if err != nil {
		logger.WithError(err).Fatal("invalid configuration")
	}
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.WithError(err).Warn("unknown LOG_LEVEL, defaulting to info")
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	client := upstream.NewClient(cfg)
	auth := upstream.NewAuthenticator(client)
	proxy := registry.NewProxy(cfg, client, auth, logger)

	gin.SetMode(gin.ReleaseMode)
	router := registry.NewRouter(cfg, proxy, logger)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: router,
	}

	go func() {
		logger.WithField("addr", cfg.ListenAddr).
			Info("lite-docker-proxy listening (plain HTTP; terminate TLS upstream)")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.WithError(err).Fatal("server failed")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.WithError(err).Error("graceful shutdown failed")
	}
}
```

- [ ] **Step 2: Build everything and run all tests**

Run:
```bash
go mod tidy
go build ./...
go test ./...
```
Expected: build succeeds, all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add main.go internal/registry/router.go go.mod go.sum
git commit -m "feat: wire gin router and server entrypoint"
```

---

### Task 8: README and manual smoke test

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write the README**

Create `README.md`:

```markdown
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
```

- [ ] **Step 2: Manual smoke test (requires network + local Docker)**

```bash
UPSTREAMS="docker.io=docker" DEFAULT_UPSTREAM=docker.io LISTEN_ADDR=:18080 go run . &
curl -i http://127.0.0.1:18080/v2/
```
Expected: `200`, `Docker-Distribution-Api-Version: registry/2.0`, body `{}`.

```bash
curl -s http://127.0.0.1:18080/v2/caddy/manifests/latest \
  -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' | head -c 200
```
Expected: a manifest JSON body (or a registry error if Docker Hub rate-limits;
a `Bearer` token is fetched automatically by the proxy).

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document configuration and usage"
```

---

## Self-Review Notes

- **Spec coverage:** env config (Task 1), alias/default mapping + Docker Hub library (Task 2), streaming + 307 passthrough (Tasks 3, 6), downstream Basic auth (Task 5), upstream bearer/basic challenge + token cache (Task 4), Gin + ginlogrus (Task 7), read-only 405 (Task 7), README/deploy note (Task 8).
- **Type consistency:** `config.Upstream`, `config.Config`, `registry.Route`, `registry.Target`, `upstream.Authenticator` signatures are used consistently across tasks.
- **Non-goals honored:** no cache, no push, single static credential pair.
