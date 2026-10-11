package registry

import "github.com/gin-gonic/gin"

type errorBody struct {
	Errors []errorEntry `json:"errors"`
}

type errorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes a Registry V2 error response.
func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, errorBody{Errors: []errorEntry{{Code: code, Message: message}}})
}
