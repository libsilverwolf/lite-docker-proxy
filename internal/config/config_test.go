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

func TestUpstreamBaseURL(t *testing.T) {
	hub := Upstream{Host: "docker.io", Alias: "docker"}
	if got := hub.BaseURL().String(); got != "https://registry-1.docker.io" {
		t.Fatalf("docker hub base URL = %q", got)
	}
	internal := Upstream{Host: "registry.internal:5000", Scheme: "http", Alias: "internal"}
	if got := internal.BaseURL().String(); got != "http://registry.internal:5000" {
		t.Fatalf("internal base URL = %q", got)
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
