// api-server is a minimal Go + gin HTTP API, used as a qooq example that
// relies on the built-in `go` Dockerfile template (build.template: go in
// qooq.yaml) instead of shipping its own Dockerfile.
package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func main() {
	r := gin.Default()
	r.GET("/hello", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "hello from api-server"})
	})
	r.Run(":8080")
}
