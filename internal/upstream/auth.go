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

// NewAuthenticator returns an Authenticator using client for token requests.
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
