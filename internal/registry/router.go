package registry

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	ginlogrus "github.com/toorop/gin-logrus"

	"github.com/libsilverwolf/lite-docker-proxy/internal/config"
)

// NewRouter builds the Gin engine with logging, recovery and registry routes.
func NewRouter(cfg *config.Config, proxy *Proxy, logger *logrus.Logger) *gin.Engine {
	r := gin.New()
	r.Use(ginlogrus.Logger(logger))
	r.Use(gin.Recovery())

	auth := AuthMiddleware(cfg.Username, cfg.Password)
	r.Any("/v2", auth, pingHandler)
	r.Any("/v2/*path", auth, dispatchHandler(cfg, proxy))
	return r
}

func pingHandler(c *gin.Context) {
	c.Header("Docker-Distribution-Api-Version", apiVersion)
	c.JSON(http.StatusOK, gin.H{})
}

func dispatchHandler(cfg *config.Config, proxy *Proxy) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Param("path")
		if p == "" || p == "/" {
			pingHandler(c)
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Header("Allow", "GET, HEAD")
			writeError(c, http.StatusMethodNotAllowed, "UNSUPPORTED", "read-only proxy: pushes are not supported")
			return
		}
		route, err := ParseRoute(p)
		if err != nil {
			writeError(c, http.StatusNotFound, "NAME_UNKNOWN", err.Error())
			return
		}
		target, ok := ResolveTarget(cfg, route.Name)
		if !ok {
			writeError(c, http.StatusNotFound, "NAME_UNKNOWN", "repository not found: no matching upstream alias and no default upstream configured")
			return
		}
		proxy.Forward(c, route, target)
	}
}
