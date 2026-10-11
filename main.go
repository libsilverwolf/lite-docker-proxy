package main

import (
	"context"
	"errors"
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

func main() {
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
