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
