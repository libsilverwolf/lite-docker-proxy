package registry

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

const basicRealm = `Basic realm="lite-docker-proxy"`

// AuthMiddleware enforces downstream Basic auth when username is non-empty.
func AuthMiddleware(username, password string) gin.HandlerFunc {
	if username == "" {
		return func(c *gin.Context) { c.Next() }
	}
	expectedUser := []byte(username)
	expectedPass := []byte(password)
	return func(c *gin.Context) {
		user, pass, ok := c.Request.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), expectedUser) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), expectedPass) != 1 {
			c.Header("WWW-Authenticate", basicRealm)
			writeError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			c.Abort()
			return
		}
		c.Next()
	}
}
