package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheckURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:9000", "http://127.0.0.1:9000/healthz"},
		{"127.0.0.1:18080", "http://127.0.0.1:18080/healthz"},
		{"", "http://127.0.0.1:8080/healthz"},
		{"8080", "http://127.0.0.1:8080/healthz"},
	}
	for _, tc := range cases {
		got, err := healthcheckURL(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("healthcheckURL(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestRunHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	var buf bytes.Buffer
	getenv := func(string) string { return strings.TrimPrefix(ok.URL, "http://") }
	if code := runHealthcheck(getenv, &buf); code != 0 {
		t.Fatalf("healthy probe code = %d, stderr = %q", code, buf.String())
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	buf.Reset()
	getenv = func(string) string { return strings.TrimPrefix(bad.URL, "http://") }
	if code := runHealthcheck(getenv, &buf); code != 1 {
		t.Fatalf("unhealthy status probe code = %d want 1", code)
	}

	buf.Reset()
	getenv = func(string) string { return "127.0.0.1:1" }
	if code := runHealthcheck(getenv, &buf); code != 1 {
		t.Fatalf("unreachable probe code = %d want 1", code)
	}
}
