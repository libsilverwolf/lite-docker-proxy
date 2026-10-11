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
