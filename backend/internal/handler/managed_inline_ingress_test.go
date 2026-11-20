//go:build unit

package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestManagedInlineIngressRejectsInlineWithoutBackend(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Files.Enabled = true
	h := &OpenAIGatewayHandler{cfg: cfg}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		groupID := int64(4)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 3, GroupID: &groupID, Group: &service.Group{ID: 4, Platform: service.PlatformOpenAI}})
		c.Next()
	})
	dispatched := false
	router.POST("/v1/responses", h.ManagedFilesIngress, func(c *gin.Context) { dispatched = true; c.Status(http.StatusNoContent) })
	body := `{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.False(t, dispatched, "inline-only requests must enter managed preparation before dispatch")
}

func TestManagedInlineIngressRejectsChatInlineBeforeDispatch(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Files.Enabled = true
	h := &OpenAIGatewayHandler{cfg: cfg}
	dispatched := false
	router := gin.New()
	router.POST("/v1/chat/completions", h.ManagedFilesIngress, func(c *gin.Context) { dispatched = true; c.Status(http.StatusNoContent) })
	body := `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"hello.txt","file_data":"aGVsbG8="}}]}]}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.False(t, dispatched, "chat inline files cannot bypass the HTTP Responses boundary")
}

func TestManagedInlineIngressRejectsMessagesTypedSourcesBeforeDispatch(t *testing.T) {
	for _, content := range []string{
		`[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]`,
		`[{"type":"tool_result","tool_use_id":"call-a","content":[{"type":"input_file","file_url":"https://example.invalid/private.txt"}]}]`,
	} {
		cfg := &config.Config{}
		cfg.Gateway.Files.Enabled = true
		h := &OpenAIGatewayHandler{cfg: cfg}
		dispatched := false
		router := gin.New()
		router.POST("/v1/messages", h.ManagedFilesIngress, func(c *gin.Context) { dispatched = true; c.Status(http.StatusNoContent) })
		body := `{"messages":[{"role":"user","content":` + content + `}]}`
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.False(t, dispatched, "typed file sources cannot bypass the HTTP Responses boundary")
		require.NotContains(t, rec.Body.String(), "private.txt")
	}
}

func TestManagedInlineWebSocketRejectsInlineInFirstAndLaterFrames(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode+"/first", func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{managedFilesEnabled: true, firstPayload: `{"type":"response.create","model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`, ingressMode: mode, firstFrameCloseExpected: true, closeReason: "Managed files require HTTP Responses"})
		})
		t.Run(mode+"/later", func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{managedFilesEnabled: true, firstPayload: `{"type":"response.create","model":"gpt-5.4","input":"hello"}`, secondPayload: `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_file","filename":"later.txt","file_data":"aGVsbG8="}]}}`, ingressMode: mode, secondTurnCloseExpected: true, closeReason: "Managed files require HTTP Responses"})
		})
	}
}

func TestManagedInlineIngressPreparesBeforeDispatchAndCleansOwnerResources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		public   bool
		fileUser int64
		encoded  string
		status   int
	}{
		{"inline only", false, 3, "aGVsbG8=", http.StatusNoContent},
		{"mixed current owner", true, 3, "aGVsbG8=", http.StatusNoContent},
		{"mixed foreign owner", true, 8, "aGVsbG8=", http.StatusNotFound},
		{"public then invalid inline", true, 3, "!invalid", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.Files = config.DefaultFilesConfig()
			cfg.Gateway.Files.Enabled = true
			cfg.Gateway.Files.Root = t.TempDir()
			repo := &managedIngressFileRepo{file: &service.StoredFile{ID: 1, PublicID: "file-private", StorageKey: "stored", UserID: tc.fileUser, GroupID: 4, State: "ready", Version: 1, Bytes: 5}}
			ledger := &managedIngressLedger{}
			files, err := service.NewFileService(repo, managedIngressBlob{}, cfg.Gateway.Files)
			require.NoError(t, err)
			h := &OpenAIGatewayHandler{cfg: cfg, managedFiles: service.NewManagedFileResolver(files, ledger, nil, cfg)}
			public := ""
			if tc.public {
				public = `{"type":"input_file","file_id":"file-private"},`
			}
			body := fmt.Sprintf(`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_file\",\"file_data\":\"!opaque\"}"},{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,opaque"},%s{"type":"input_file","filename":"inline.txt","file_data":%q}]}],"screenshot_sidecar":{"scope":"sheet-a","turn":9}}`, public, tc.encoded)
			router := gin.New()
			router.Use(func(c *gin.Context) {
				groupID := int64(4)
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 3, GroupID: &groupID, Group: &service.Group{ID: 4, Platform: service.PlatformOpenAI}})
				c.Next()
			})
			dispatched := false
			router.POST("/v1/responses", h.ManagedFilesIngress, func(c *gin.Context) {
				dispatched = true
				require.NotNil(t, service.ManagedFilePlanFromContext(c.Request.Context()))
				got, err := readLenientJSONRequestBodyWithPrealloc(c.Request, cfg)
				require.NoError(t, err)
				require.Equal(t, body, string(got), "opaque fields, image and original turn positions must stay unchanged")
				entries, err := os.ReadDir(cfg.Gateway.Files.Root)
				require.NoError(t, err)
				require.Len(t, entries, 1, "inline temporary blob must remain live until dispatch finishes")
				_, released := repo.readActivity()
				require.Zero(t, released)
				c.Status(http.StatusNoContent)
			})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Equal(t, tc.status == http.StatusNoContent, dispatched)
			opened, released := repo.readActivity()
			if tc.public && dispatched {
				require.Equal(t, 1, opened)
				require.Equal(t, 1, released)
				require.Equal(t, []service.FileOwner{{UserID: 3, APIKeyID: 2, GroupID: 4}}, ledger.owners)
			} else {
				require.Zero(t, opened)
				require.Zero(t, released)
				require.Empty(t, ledger.owners)
			}
			entries, err := os.ReadDir(cfg.Gateway.Files.Root)
			require.NoError(t, err)
			require.Empty(t, entries, "request-local inline storage must close on success and rejection")
		})
	}
}

func TestManagedInlineIngressRejectsUnsupportedResponsesSources(t *testing.T) {
	for _, input := range []string{
		`[{"type":"function_call_output","call_id":"call-a","output":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]`,
		`[{"role":"user","content":[{"type":"input_file","file_url":"https://example.invalid/private.txt"}]}]`,
	} {
		cfg := &config.Config{}
		cfg.Gateway.Files.Enabled = true
		h := &OpenAIGatewayHandler{cfg: cfg}
		dispatched := false
		router := gin.New()
		router.Use(func(c *gin.Context) {
			groupID := int64(4)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 3, GroupID: &groupID, Group: &service.Group{ID: 4, Platform: service.PlatformOpenAI}})
			c.Next()
		})
		router.POST("/v1/responses", h.ManagedFilesIngress, func(c *gin.Context) { dispatched = true; c.Status(http.StatusNoContent) })
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":`+input+`}`)))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.False(t, dispatched)
		require.NotContains(t, rec.Body.String(), "private.txt")
	}
}

func TestManagedInlineIngressPreservesUnmanagedProtocolBodies(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"input":[{"type":"function_call","arguments":"{\"type\":\"input_file\",\"file_data\":\"!opaque\"}"},{"type":"custom_tool_call","input":"{\"type\":\"input_file\",\"file_url\":\"opaque\"}"},{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,opaque"}]}],"ordinary":1,"ordinary":2}`},
		{"/v1/messages", `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"opaque"}},{"type":"document","source":{"type":"file","file_id":"file_native"}}]}]}`},
	} {
		cfg := &config.Config{}
		cfg.Gateway.Files.Enabled = true
		h := &OpenAIGatewayHandler{cfg: cfg}
		router := gin.New()
		dispatched := false
		router.POST(tc.path, h.ManagedFilesIngress, func(c *gin.Context) {
			dispatched = true
			got, err := readLenientJSONRequestBodyWithPrealloc(c.Request, cfg)
			require.NoError(t, err)
			require.Equal(t, tc.body, string(got))
			require.Nil(t, service.ManagedFilePlanFromContext(c.Request.Context()))
			c.Status(http.StatusNoContent)
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		require.True(t, dispatched)
	}
}
