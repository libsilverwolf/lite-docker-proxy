package upstream

import (
	"net/http"
	"testing"
	"time"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

func TestNewClientRedirectsAreNotFollowed(t *testing.T) {
	cfg := &config.Config{UpstreamHeaderTimeout: 5 * time.Second}

	client := NewClient(cfg)
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	err := client.CheckRedirect(&http.Request{}, nil)
	if err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect error = %v, want ErrUseLastResponse", err)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if !transport.DisableCompression {
		t.Fatal("DisableCompression should be true for a transparent proxy")
	}
	if transport.ResponseHeaderTimeout != 5*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v", transport.ResponseHeaderTimeout)
	}
	if transport.TLSClientConfig != nil {
		t.Fatal("TLSClientConfig should be nil when insecure is disabled")
	}
}

func TestNewClientTLSInsecure(t *testing.T) {
	client := NewClient(&config.Config{UpstreamTLSInsecure: true, UpstreamHeaderTimeout: time.Second})
	transport := client.Transport.(*http.Transport)
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify transport when UpstreamTLSInsecure is set")
	}
}
