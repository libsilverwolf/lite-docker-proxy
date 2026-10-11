package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
	"github.com/libsilverwolf/lite-docker-proxy/internal/registry"
	"github.com/libsilverwolf/lite-docker-proxy/internal/upstream"
)

const healthPath = "/healthz"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Getenv, os.Stderr))
	}
	runServer()
}

func runServer() {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	cfg, err := config.Load()
	if err != nil {
		logger.WithError(err).Fatal("invalid configuration")
	}
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.WithError(err).Warn("unknown LOG_LEVEL, defaulting to info")
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	client := upstream.NewClient(cfg)
	auth := upstream.NewAuthenticator(client)
	proxy := registry.NewProxy(cfg, client, auth, logger)

	gin.SetMode(gin.ReleaseMode)
	router := registry.NewRouter(cfg, proxy, logger)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: router,
	}

	go func() {
		logger.WithField("addr", cfg.ListenAddr).
			Info("lite-docker-proxy listening (plain HTTP; terminate TLS upstream)")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.WithError(err).Fatal("server failed")
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.WithError(err).Error("graceful shutdown failed")
	}
}

// runHealthcheck performs a single GET against the local health endpoint and
// returns a process exit code. It is used by the container HEALTHCHECK so the
// runtime image does not need curl or wget.
func runHealthcheck(getenv func(string) string, stderr io.Writer) int {
	target, err := healthcheckURL(getenv("LISTEN_ADDR"))
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "healthcheck: %s returned %d\n", target, resp.StatusCode)
		return 1
	}
	return 0
}

// healthcheckURL derives the local health URL from LISTEN_ADDR.
func healthcheckURL(listenAddr string) (string, error) {
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Allow a bare port, e.g. "8080".
		host, port = "", listenAddr
	}
	if port == "" {
		return "", fmt.Errorf("cannot determine port from LISTEN_ADDR %q", listenAddr)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + healthPath, nil
}
