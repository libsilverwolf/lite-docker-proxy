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
	defaultListenAddr    = ":8080"
	defaultLogLevel      = "info"
	defaultHeaderTimeout = 60 * time.Second
	defaultScheme        = "https"
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

// UpstreamByAlias looks up a whitelisted upstream by its alias prefix.
func (c *Config) UpstreamByAlias(alias string) (Upstream, bool) {
	for _, u := range c.Upstreams {
		if u.Alias == alias {
			return u, true
		}
	}
	return Upstream{}, false
}

// UpstreamByHost looks up a whitelisted upstream by its host.
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
