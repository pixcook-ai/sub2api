package handler

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Only the server-owned preparation marker can bypass upstream health and retry
// handling. A matching public error string or an unmarked cause is insufficient.
func (h *OpenAIGatewayHandler) handleManagedFilePreparationError(c *gin.Context, err error, streamStarted bool) bool {
	var preparation *service.ManagedFilePreparationError
	if !errors.As(err, &preparation) {
		return false
	}
	if service.IsResponseCommitted(c) {
		return true
	}
	status, kind, code, message := http.StatusServiceUnavailable, "server_error", "managed_files_unavailable", "Managed file preparation is temporarily unavailable"
	switch {
	case errors.Is(preparation.Err, service.ErrBPSRequestAuthorizationRevoked), errors.Is(preparation.Err, service.ErrFileAuthorizationRevoked):
		status, kind, code, message = http.StatusForbidden, "permission_denied", "permission_denied", "Request authorization is no longer valid"
	case errors.Is(preparation.Err, service.ErrBPSRequestAuthorizationUnavailable):
		message = "Request authorization is temporarily unavailable"
	case errors.Is(preparation.Err, service.ErrFileNotFound):
		status, kind, code, message = http.StatusNotFound, "invalid_request_error", "file_not_found", "File not found"
	case errors.Is(preparation.Err, service.ErrManagedFileBusy):
		status, kind, code, message = http.StatusConflict, "conflict_error", "managed_file_busy", "Managed file is busy"
	case errors.Is(preparation.Err, service.ErrManagedFileBindingConflict):
		status, kind, code, message = http.StatusConflict, "conflict_error", "managed_file_binding_conflict", "Managed file binding conflicts with this request"
	case errors.Is(preparation.Err, service.ErrManagedFileUncertain):
		status, kind, code, message = http.StatusConflict, "conflict_error", "managed_file_uncertain", "Managed file upload outcome is uncertain"
	case errors.Is(preparation.Err, service.ErrFileLeaseConflict):
		status, kind, code, message = http.StatusConflict, "conflict_error", "file_lease_conflict", "File operation conflicts with its current lease"
	case errors.Is(preparation.Err, service.ErrManagedFileRecoveryBudget):
		status, kind, code, message = http.StatusTooManyRequests, "rate_limit_error", "managed_file_recovery_budget_exceeded", "Managed file recovery budget is exhausted"
	case errors.Is(preparation.Err, service.ErrManagedFileUnsupportedInput):
		status, kind, code, message = http.StatusBadRequest, "invalid_request_error", "managed_files_unsupported", "The managed file input is not supported"
	case errors.Is(preparation.Err, service.ErrManagedFileGenerationReplay):
		status, kind, code, message = http.StatusConflict, "conflict_error", "managed_file_generation_replay", "Managed file generation cannot be replayed"
	}
	h.handleStreamingAwareErrorWithCode(c, status, kind, code, message, streamStarted || c.Writer.Written(), false)
	service.MarkResponseCommitted(c)
	return true
}
