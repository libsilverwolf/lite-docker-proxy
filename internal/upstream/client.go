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
