package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestStoredResponseRoutesAreAbsentWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := service.DefaultStoredResponseConfig()
	svc, err := service.NewStoredResponseService(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*handler.StoredResponseHandler{nil, handler.NewStoredResponseHandler(nil), handler.NewStoredResponseHandler(svc)} {
		r := gin.New()
		authCalls := 0
		RegisterStoredResponseRoutes(r, h, middleware.APIKeyAuthMiddleware(func(c *gin.Context) { authCalls++; c.Next() }))
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, "/v1/responses/response-id", nil))
			if w.Code != http.StatusNotFound || authCalls != 0 {
				t.Fatalf("disabled route mounted: status=%d auth=%d", w.Code, authCalls)
			}
		}
	}
}

func TestStoredResponseRoutesUseSharedAuthAndPreserveBareResponsesEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := service.DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, err := service.NewStoredResponseService(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.GET("/v1/responses", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusAccepted) })
	authCalls := 0
	RegisterStoredResponseRoutes(r, handler.NewStoredResponseHandler(svc), middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		authCalls++
		c.AbortWithStatus(http.StatusUnauthorized)
	}))
	for _, tc := range []struct {
		method, path string
		status, auth int
	}{
		{http.MethodGet, "/v1/responses", http.StatusNoContent, 0},
		{http.MethodPost, "/v1/responses", http.StatusAccepted, 0},
		{http.MethodGet, "/v1/responses/response-id", http.StatusUnauthorized, 1},
		{http.MethodDelete, "/v1/responses/response-id", http.StatusUnauthorized, 2},
		{http.MethodPost, "/v1/responses/response-id", http.StatusNotFound, 2},
		{http.MethodGet, "/v1/responses/response-id/input_items", http.StatusNotFound, 2},
		{http.MethodPost, "/v1/responses/response-id/cancel", http.StatusNotFound, 2},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || authCalls != tc.auth {
			t.Fatalf("%s %s status=%d auth=%d; want status=%d auth=%d", tc.method, tc.path, w.Code, authCalls, tc.status, tc.auth)
		}
	}
}
