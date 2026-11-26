package watcher

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// openapiSpec is the OpenAPI 3.0 spec for qooq-watcher's REST API, co-located
// with the router it describes so the two can't drift apart silently.
//
//go:embed openapi.yaml
var openapiSpec []byte

// swaggerUIHTML loads swagger-ui-dist from a CDN rather than vendoring it:
// this is a docs viewer, not a runtime dependency. The version is pinned and
// both assets carry Subresource Integrity hashes + crossorigin (same pin as
// qooqd's internal/api/docs.go), so a substituted CDN asset is rejected by
// the browser. Bump the version and both sha384 hashes together when upgrading.
const swaggerUIHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>qooq-watcher API docs</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css" integrity="sha384-wxLW6kwyHktdDGr6Pv1zgm/VGJh99lfUbzSn6HNHBENZlCN7W602k9VkGdxuFvPn" crossorigin="anonymous">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" integrity="sha384-wmyclcVGX/WhUkdkATwhaK1X1JtiNrr2EoYJ+diV3vj4v6OC5yCeSu+yW13SYJep" crossorigin="anonymous"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: "/openapi.yaml",
        dom_id: "#swagger-ui",
      });
    };
  </script>
</body>
</html>
`

func (w *Watcher) handleOpenAPIYAML(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml", openapiSpec)
}

func (w *Watcher) handleDocsUI(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(swaggerUIHTML))
}
