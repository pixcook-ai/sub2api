package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// Files have no model and must not enter model allowlists or composite routing.
func RegisterFileRoutes(r *gin.Engine, h *handler.FileHandler, auth middleware.APIKeyAuthMiddleware) {
	if !h.Enabled() {
		return
	}
	files := r.Group("/v1/files")
	files.Use(gin.HandlerFunc(auth))
	files.GET("", h.List)
	files.POST("", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.MaxUploadBodyBytes())
		h.Upload(c)
	})
	files.GET("/:file_id", h.Get)
	files.DELETE("/:file_id", h.Delete)
	files.GET("/:file_id/content", h.Content)
}
