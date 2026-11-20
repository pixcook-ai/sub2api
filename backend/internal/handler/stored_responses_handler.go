package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type StoredResponseHandler struct {
	responses *service.StoredResponseService
}

func NewStoredResponseHandler(responses *service.StoredResponseService) *StoredResponseHandler {
	return &StoredResponseHandler{responses: responses}
}

func (h *StoredResponseHandler) Enabled() bool {
	return h != nil && h.responses.Enabled()
}

func storedResponseOwner(c *gin.Context) (service.StoredResponseOwner, bool) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 || key.Group == nil || key.Group.ID != *key.GroupID {
		storedResponseError(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required", "permission_denied")
		return service.StoredResponseOwner{}, false
	}
	return service.StoredResponseOwner{UserID: key.UserID, APIKeyID: key.ID, GroupID: *key.GroupID}, true
}

func (h *StoredResponseHandler) Retrieve(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if c.Request.URL.RawQuery != "" {
		storedResponseError(c, http.StatusBadRequest, "invalid_request_error", "Response retrieval query parameters are not supported", "invalid_request_error")
		return
	}
	owner, ok := storedResponseOwner(c)
	if !ok {
		return
	}
	record, err := h.responses.Get(c.Request.Context(), owner, c.Param("response_id"))
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if record == nil || len(record.PublicResponse) == 0 {
		storedResponseUnavailable(c)
		return
	}
	if !storedResponseModelAllowed(c, record.Model) {
		return
	}
	// StoredResponse contains backend binding and lease metadata. Return only its
	// separately validated public object, never serialize the private record.
	c.Data(http.StatusOK, "application/json", record.PublicResponse)
}

func (h *StoredResponseHandler) Delete(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if c.Request.URL.RawQuery != "" {
		storedResponseError(c, http.StatusBadRequest, "invalid_request_error", "Response deletion query parameters are not supported", "invalid_request_error")
		return
	}
	owner, ok := storedResponseOwner(c)
	if !ok {
		return
	}
	id := c.Param("response_id")
	model, err := h.responses.GetManagementModel(c.Request.Context(), owner, id)
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if !storedResponseModelAllowed(c, model) {
		return
	}
	if err := h.responses.Delete(c.Request.Context(), owner, id); err != nil {
		handleStoredResponseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "object": "response.deleted", "deleted": true})
}

func storedResponseModelAllowed(c *gin.Context, model string) bool {
	if strings.TrimSpace(model) == "" {
		storedResponseUnavailable(c)
		return false
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil {
		storedResponseError(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required", "permission_denied")
		return false
	}
	if !key.Group.ModelAllowlist.Allows(model) {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		middleware.MarkIngressRejected(c, middleware.IngressRejectModelNotAllowed)
		middleware.OpenAIErrorWriter(c, http.StatusNotFound, fmt.Sprintf("Model %q is not available for this group", model))
		c.Abort()
		return false
	}
	return true
}

func storedResponseError(c *gin.Context, status int, kind, message, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": kind, "param": nil, "code": code}})
}

func storedResponseUnavailable(c *gin.Context) {
	storedResponseError(c, http.StatusServiceUnavailable, "server_error", "Response storage is temporarily unavailable", "response_storage_unavailable")
}

func handleStoredResponseError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrStoredResponseNotFound):
		storedResponseError(c, http.StatusNotFound, "invalid_request_error", "Response not found", "response_not_found")
	case errors.Is(err, service.ErrStoredResponseInvalid), errors.Is(err, service.ErrStoredResponseUnsupported):
		storedResponseError(c, http.StatusBadRequest, "invalid_request_error", "Invalid stored response request", "invalid_request_error")
	case errors.Is(err, service.ErrStoredResponseQuota):
		storedResponseError(c, http.StatusTooManyRequests, "rate_limit_error", "Response storage capacity is exhausted", "response_storage_quota_exceeded")
	case errors.Is(err, service.ErrStoredResponseTooLarge):
		storedResponseError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Response exceeds the configured storage limit", "response_too_large")
	case errors.Is(err, service.ErrStoredResponseConflict):
		storedResponseError(c, http.StatusConflict, "conflict_error", "Response operation conflicts with its current state", "response_conflict")
	default:
		storedResponseUnavailable(c)
	}
}
