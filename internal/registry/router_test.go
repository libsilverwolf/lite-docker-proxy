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

// newTestRouter wires a real Gin router against a fake upstream server and
// returns the router plus the upstream host used in the whitelist.
func newTestRouter(t *testing.T, upstreamHandler http.Handler, mutate func(*config.Config)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(upstreamHandler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		DefaultUpstream: u.Host,
		Upstreams: []config.Upstream{
			{Host: u.Host, Scheme: "http", Alias: "test"},
			{Host: "docker.io", Scheme: "https", Alias: "docker"},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}

	client := upstream.NewClient(cfg)
	proxy := NewProxy(cfg, client, upstream.NewAuthenticator(client), logrus.New())
	return NewRouter(cfg, proxy, logrus.New())
}

func doRequest(t *testing.T, r *gin.Engine, method, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRouterPingRequiresAuth(t *testing.T) {
	r := newTestRouter(t, http.NotFoundHandler(), func(cfg *config.Config) {
		cfg.Username, cfg.Password = "admin", "secret"
	})

	rec := doRequest(t, r, http.MethodGet, "/v2/", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatalf("missing WWW-Authenticate challenge")
	}

	rec = doRequest(t, r, http.MethodGet, "/v2/", "admin", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-pass status = %d want 401", rec.Code)
	}

	rec = doRequest(t, r, http.MethodGet, "/v2/", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d want 200", rec.Code)
	}
	if got := rec.Header().Get("Docker-Distribution-Api-Version"); got != "registry/2.0" {
		t.Fatalf("api version header = %q", got)
	}
}

func TestRouterForwardsAliasToUpstream(t *testing.T) {
	var gotPath string
	r := newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		_, _ = io.WriteString(w, "manifest")
	}), nil)

	rec := doRequest(t, r, http.MethodGet, "/v2/test/owner/repo/manifests/latest", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "manifest" {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if gotPath != "/v2/owner/repo/manifests/latest" {
		t.Fatalf("upstream path = %q", gotPath)
	}
}

func TestRouterDefaultUpstreamForBareName(t *testing.T) {
	var gotPath string
	r := newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		w.WriteHeader(http.StatusOK)
	}), nil)

	rec := doRequest(t, r, http.MethodGet, "/v2/caddy/manifests/latest", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// The fake upstream is not Docker Hub, so no "library/" prefix is added here;
	// that expansion is covered by TestResolveTarget.
	if gotPath != "/v2/caddy/manifests/latest" {
		t.Fatalf("upstream path = %q", gotPath)
	}
}

func TestRouterHealthzSkipsAuth(t *testing.T) {
	r := newTestRouter(t, http.NotFoundHandler(), func(cfg *config.Config) {
		cfg.Username, cfg.Password = "admin", "secret"
	})
	rec := doRequest(t, r, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /healthz status = %d want 200", rec.Code)
	}
}

func TestRouterRejectsPush(t *testing.T) {
	r := newTestRouter(t, http.NotFoundHandler(), nil)
	rec := doRequest(t, r, http.MethodPost, "/v2/test/blobs/uploads/", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d want 405", rec.Code)
	}
}

func TestRouterUnknownPathReturns404(t *testing.T) {
	r := newTestRouter(t, http.NotFoundHandler(), nil)
	if rec := doRequest(t, r, http.MethodGet, "/v2/test/onlyrepo", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404", rec.Code)
	}
}

func TestRouterAliasWithoutDefault(t *testing.T) {
	r := newTestRouter(t, http.NotFoundHandler(), func(cfg *config.Config) {
		cfg.DefaultUpstream = ""
	})
	if rec := doRequest(t, r, http.MethodGet, "/v2/caddy/manifests/latest", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bare name with no default: status = %d want 404", rec.Code)
	}

	var gotPath string
	r = newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
	}), func(cfg *config.Config) {
		cfg.DefaultUpstream = ""
	})
	if rec := doRequest(t, r, http.MethodGet, "/v2/test/repo/manifests/latest", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("alias with no default: status = %d want 200", rec.Code)
	}
	if gotPath != "/v2/repo/manifests/latest" {
		t.Fatalf("upstream path = %q", gotPath)
	}
}
