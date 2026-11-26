// Command server is the fullstack-server example: a gin API (GET /api/hello)
// plus the go:embed'd Vite frontend served for every other path. Used with
// qooq.yaml's build.template: go-react (no Dockerfile in this repo).
package main

import (
	"io/fs"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"example.com/qooq-examples/fullstack-server/internal/webui"
)

func main() {
	dist, err := fs.Sub(webui.FS, "dist")
	if err != nil {
		log.Fatalf("sub dist: %v", err)
	}

	r := gin.Default()
	r.GET("/api/hello", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "hello from fullstack-server"})
	})
	r.NoRoute(gin.WrapH(http.FileServer(http.FS(dist))))

	if err := r.Run(":8080"); err != nil {
		log.Fatal(err)
	}
}
