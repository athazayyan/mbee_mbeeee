package calculator

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// Routes registers the public calculator. It needs no account and stores nothing.
func Routes(api *gin.RouterGroup, origin string) {
	group := api.Group("")
	group.Use(httpapi.RateLimit(120, time.Minute))
	if origin != "" {
		group.Use(httpapi.RequireOrigin(origin))
	}
	group.POST("/calculators/break-even", breakEven)
}

func breakEven(c *gin.Context) {
	input, ok := httpapi.ReadJSON[Input](c, 2<<10)
	if !ok {
		return
	}
	if fields := Validate(input); fields != nil {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa angka yang diisi.", fields)
		return
	}
	httpapi.Success(c, http.StatusOK, Calculate(input))
}
