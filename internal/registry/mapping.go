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
