package handler

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type FileHandler struct{ files *service.FileService }

func NewFileHandler(files *service.FileService) *FileHandler { return &FileHandler{files: files} }
func (h *FileHandler) Enabled() bool                         { return h != nil && h.files.Enabled() }
func (h *FileHandler) MaxUploadBodyBytes() int64             { return h.files.MaxUploadBodyBytes() }
func fileOwner(c *gin.Context) (service.FileOwner, bool) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.GroupID == nil {
		fileError(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required")
		return service.FileOwner{}, false
	}
	return service.FileOwner{UserID: key.UserID, APIKeyID: key.ID, GroupID: *key.GroupID}, true
}
func fileObject(f *service.StoredFile) gin.H {
	return gin.H{"id": f.PublicID, "object": "file", "bytes": f.Bytes, "created_at": f.CreatedAt.Unix(), "filename": f.Filename, "purpose": f.Purpose}
}
func (h *FileHandler) Upload(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	reader, err := c.Request.MultipartReader()
	if err != nil {
		fileError(c, 400, "invalid_request_error", "Expected multipart/form-data")
		return
	}
	f, err := h.files.UploadMultipart(c.Request.Context(), owner, reader)
	if err != nil {
		handleFileError(c, err)
		return
	}
	c.JSON(http.StatusOK, fileObject(f))
}
func (h *FileHandler) Get(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	f, err := h.files.Get(c.Request.Context(), owner, c.Param("file_id"))
	if err != nil {
		handleFileError(c, err)
		return
	}
	c.JSON(http.StatusOK, fileObject(f))
}
func (h *FileHandler) Content(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	f, reader, err := h.files.Open(c.Request.Context(), owner, c.Param("file_id"))
	if err != nil {
		handleFileError(c, err)
		return
	}
	defer reader.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
	c.Header("Content-Length", strconv.FormatInt(f.Bytes, 10))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-SHA256", f.SHA256)
	c.Status(http.StatusOK)
	_, _ = io.CopyBuffer(c.Writer, reader, make([]byte, 32*1024))
}
func fileError(c *gin.Context, status int, kind, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": kind, "param": nil, "code": nil}})
}
func handleFileError(c *gin.Context, err error) {
	var bodyTooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, service.ErrFileNotFound):
		fileError(c, 404, "invalid_request_error", "File not found")
	case errors.Is(err, service.ErrFileTooLarge) || errors.As(err, &bodyTooLarge):
		fileError(c, 413, "invalid_request_error", "File exceeds the configured upload limit")
	case errors.Is(err, service.ErrInvalidFile):
		fileError(c, 400, "invalid_request_error", err.Error())
	case errors.Is(err, service.ErrFileQuotaExceeded) || errors.Is(err, service.ErrFileStorageFull):
		fileError(c, 429, "storage_quota_exceeded", "File storage capacity is exhausted")
	case errors.Is(err, service.ErrFileLeaseConflict):
		fileError(c, 409, "conflict_error", "File operation lease expired; retry the operation")
	default:
		fileError(c, 500, "server_error", "File storage is temporarily unavailable")
	}
}
