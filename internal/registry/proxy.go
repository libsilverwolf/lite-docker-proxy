package registry

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
	"github.com/libsilverwolf/lite-docker-proxy/internal/upstream"
)

const apiVersion = "registry/2.0"

var (
	errNilBaseURL = errors.New("target base URL is nil")

	hopHeaders = []string{
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
)

// Proxy forwards Registry V2 requests to upstream registries.
type Proxy struct {
	cfg    *config.Config
	client *http.Client
	auth   *upstream.Authenticator
	logger *logrus.Logger
}

// NewProxy builds a Proxy. A nil logger falls back to a default logger.
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
		return "", errNilBaseURL
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
		c.Writer.WriteHeaderNow()
		return
	}
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		_ = c.Error(err)
	}
	c.Writer.WriteHeaderNow()
}
