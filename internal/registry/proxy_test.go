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
