//go:build unit

package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayManagedFilesGuardCoversAliasesBeforeDispatch(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Files.Enabled = true
	cfg.Gateway.MaxBodySize = 1 << 20
	cfg.Gateway.TextMaxBodySize = 1 << 20
	h := &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: handler.NewOpenAIGatewayHandler(nil, nil, nil, nil, nil, nil, nil, nil, cfg), AsyncImage: handler.NewAsyncImageHandler(nil, nil)}
	router := gin.New()
	RegisterGatewayRoutes(router, h, servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		groupID := int64(4)
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 3, GroupID: &groupID, Group: &service.Group{ID: 4, Platform: service.PlatformAnthropic}})
		c.Next()
	}), nil, nil, nil, nil, nil, cfg)
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses", "/v1/responses/compact", "/responses/compact", "/backend-api/codex/responses/compact", "/v1/chat/completions", "/chat/completions", "/v1/messages", "/antigravity/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			body := `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}],"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-private"}},{"type":"input_file","file_id":"file-private"}]}]}`
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "managed_files_unsupported")
			require.NotContains(t, rec.Body.String(), "file-private")
		})
	}
}
