package apidocs

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed openapi.yaml
var specification []byte

//go:embed reference.html
var reference []byte

func Routes(router *gin.Engine) {
	router.GET("/api/openapi.yaml", func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", specification)
	})
	router.GET("/api-reference", func(c *gin.Context) {
		c.Header("X-Robots-Tag", "noindex")
		c.Data(http.StatusOK, "text/html; charset=utf-8", reference)
	})
}
