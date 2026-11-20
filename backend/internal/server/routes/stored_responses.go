package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// Stored resources use shared API-key authorization and authorize their stored
// client model in the handler. They never enter inference or account scheduling.
func RegisterStoredResponseRoutes(r *gin.Engine, h *handler.StoredResponseHandler, auth middleware.APIKeyAuthMiddleware) {
	if !h.Enabled() {
		return
	}
	responses := r.Group("/v1/responses")
	responses.Use(gin.HandlerFunc(auth))
	responses.GET("/:response_id", h.Retrieve)
	responses.DELETE("/:response_id", h.Delete)
}
