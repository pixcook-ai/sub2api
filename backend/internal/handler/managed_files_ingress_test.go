//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestManagedFilesIngressStripsPrivateHeadersBeforeDispatch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			t.Run(method+"/enabled="+map[bool]string{false: "false", true: "true"}[enabled], func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Gateway.Files.Enabled = enabled
				h := &OpenAIGatewayHandler{cfg: cfg}
				headers := []string{managedfiles.ServiceKeyHeader, managedfiles.AdmissionHeader, managedfiles.OwnerRefHeader, managedfiles.GroupRefHeader, managedfiles.BindingHeader}
				r := gin.New()
				r.Handle(method, "/v1/responses", h.ManagedFilesIngress, func(c *gin.Context) {
					for _, header := range headers {
						require.Empty(t, c.Request.Header.Values(header), header)
					}
					c.Status(http.StatusNoContent)
				})
				req := httptest.NewRequest(method, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
				for _, header := range headers {
					req.Header.Add(header, "untrusted-private-value")
				}
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				require.Equal(t, http.StatusNoContent, rec.Code)
			})
		}
	}
}

func TestManagedFilesIngressRejectsTypedFilesBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Gateway.Files.Enabled = true
	h := &OpenAIGatewayHandler{cfg: cfg}
	called := false
	router := gin.New()
	router.Use(func(c *gin.Context) {
		groupID := int64(4)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 3, GroupID: &groupID, Group: &service.Group{ID: 4, Platform: service.PlatformAnthropic}})
		c.Next()
	})
	router.POST("/v1/responses", h.ManagedFilesIngress, func(c *gin.Context) { called = true; c.Status(http.StatusNoContent) })
	body := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.False(t, called)
	require.NotContains(t, rec.Body.String(), "file-private")
}

func TestManagedFilesIngressPreservesNativeMessagesAndCachedBody(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Files.Enabled = true
	h := &OpenAIGatewayHandler{cfg: cfg}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_native"}}]}]}`)
	router := gin.New()
	router.POST("/v1/messages", h.ManagedFilesIngress, func(c *gin.Context) {
		got, err := readLenientJSONRequestBodyWithPrealloc(c.Request, cfg)
		require.NoError(t, err)
		require.JSONEq(t, string(body), string(got))
		c.Status(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestManagedFilesWebSocketRejectsFirstAndLaterTypedFiles(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode+"/first", func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{managedFilesEnabled: true, firstPayload: `{"type":"response.create","model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}`, ingressMode: mode, firstFrameCloseExpected: true, closeReason: "Managed files require HTTP Responses"})
		})
		t.Run(mode+"/later", func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{managedFilesEnabled: true, firstPayload: `{"type":"response.create","model":"gpt-5.4","input":"hello"}`, secondPayload: `{"type":"response.create","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}`, ingressMode: mode, secondTurnCloseExpected: true, closeReason: "Managed files require HTTP Responses"})
		})
	}
}

func TestManagedFilesIngressProtocolBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		enabled          bool
		status           int
	}{
		{"chat file", "/v1/chat/completions", `{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-private"}}]}]}`, true, 400},
		{"messages OpenAI file", "/v1/messages", `{"messages":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}`, true, 400},
		{"chat duplicate type", "/v1/chat/completions", `{"messages":[{"role":"user","content":[{"type":"text","type":"file","file":{"file_id":"file-private"}}]}]}`, true, 400},
		{"chat repeated messages", "/v1/chat/completions", `{"messages":[],"messages":[{"content":[{"type":"file","file":{"file_id":"file-private"}}]}]}`, true, 400},
		{"chat uppercase aliases", "/v1/chat/completions", `{"Messages":[{"Content":[{"Type":"file","File":{"File_ID":"file-private"}}]}]}`, true, 400},
		{"messages tool-result file", "/v1/messages", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a","content":[{"type":"input_file","file_id":"file-private"}]}]}]}`, true, 400},
		{"native base64", "/v1/messages", `{"messages":[{"content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA=="}}]}]}`, true, 204},
		{"native url", "/v1/messages", `{"messages":[{"content":[{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}]}]}`, true, 204},
		{"inline image", "/v1/responses", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`, true, 204},
		{"tool arguments opaque", "/v1/chat/completions", `{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"file_id\":\"file-private\"}"}}]}]}`, true, 204},
		{"feature off", "/v1/chat/completions", `{"messages":[{"content":[{"type":"file","file":{"file_id":"file-private"}}]}]}`, false, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.Files.Enabled = tc.enabled
			h := &OpenAIGatewayHandler{cfg: cfg}
			r := gin.New()
			r.POST(tc.path, h.ManagedFilesIngress, func(c *gin.Context) { c.Status(204) })
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body)))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
		})
	}
}

type managedIngressFileRepo struct {
	service.FileRepository
	mu               sync.Mutex
	file             *service.StoredFile
	opened, released int
}

func (r *managedIngressFileRepo) GetForOwner(_ context.Context, user, group int64, id string) (*service.StoredFile, error) {
	if r.file == nil || r.file.PublicID != id || r.file.UserID != user || r.file.GroupID != group {
		return nil, service.ErrFileNotFound
	}
	copy := *r.file
	return &copy, nil
}
func (r *managedIngressFileRepo) AcquireReadGrant(ctx context.Context, user, group int64, id, token string, lease, _ time.Duration) (*service.StoredFile, service.FileReadGrant, error) {
	f, e := r.GetForOwner(ctx, user, group, id)
	if e == nil {
		r.mu.Lock()
		r.opened++
		r.mu.Unlock()
	}
	now := time.Now()
	return f, service.FileReadGrant{DBNow: now, Until: now.Add(lease)}, e
}
func (r *managedIngressFileRepo) ReleaseRead(context.Context, int64, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released++
	return nil
}
func (r *managedIngressFileRepo) RenewReadGrant(_ context.Context, _ int64, _ string, lease, _ time.Duration) (service.FileReadGrant, error) {
	now := time.Now()
	return service.FileReadGrant{DBNow: now, Until: now.Add(lease)}, nil
}
func (r *managedIngressFileRepo) readActivity() (opened, released int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened, r.released
}

type managedIngressBlob struct{ service.FileBlobStore }

func (managedIngressBlob) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("hello")), nil
}

type managedIngressLedger struct {
	service.ManagedFileRepository
	owners []service.FileOwner
}

func (l *managedIngressLedger) LookupBindings(_ context.Context, o service.FileOwner, _ []string) (service.ManagedFileLookup, error) {
	l.owners = append(l.owners, o)
	return service.ManagedFileLookup{}, nil
}

func TestManagedFilesIngressPreparesOwnerAndClosesReadLease(t *testing.T) {
	for _, tc := range []struct {
		name                string
		fileUser, fileGroup int64
		status              int
	}{{"same current owner", 3, 4, 204}, {"foreign user", 8, 4, 404}, {"old group", 3, 8, 404}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.Files = config.DefaultFilesConfig()
			cfg.Gateway.Files.Enabled = true
			cfg.Gateway.Files.Root = t.TempDir()
			repo := &managedIngressFileRepo{file: &service.StoredFile{ID: 1, PublicID: "file-private", StorageKey: "stored", UserID: tc.fileUser, GroupID: tc.fileGroup, State: "ready", Version: 1, Bytes: 5}}
			ledger := &managedIngressLedger{}
			files, e := service.NewFileService(repo, managedIngressBlob{}, cfg.Gateway.Files)
			require.NoError(t, e)
			h := &OpenAIGatewayHandler{cfg: cfg, managedFiles: service.NewManagedFileResolver(files, ledger, nil, cfg)}
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
				_, released := repo.readActivity()
				require.Zero(t, released, "lease must survive until forwarding ends")
				c.Status(204)
			})
			body := `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}`
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			opened, released := repo.readActivity()
			if tc.status == 204 {
				require.True(t, dispatched)
				require.Equal(t, []service.FileOwner{{UserID: 3, APIKeyID: 2, GroupID: 4}}, ledger.owners)
				require.Equal(t, 1, opened)
				require.Equal(t, 1, released)
			} else {
				require.False(t, dispatched)
				require.Zero(t, opened)
				require.Empty(t, ledger.owners)
			}
		})
	}
}

func TestManagedFilesWebSocketRejectsNonCreateFileFrames(t *testing.T) {
	for _, mode := range []struct {
		name, ingress string
		raw           bool
	}{{"passthrough", service.OpenAIWSIngressModePassthrough, false}, {"dedicated", service.OpenAIWSIngressModeDedicated, false}, {"raw", service.OpenAIWSIngressModePassthrough, true}} {
		for _, tc := range []struct{ name, payload string }{
			{"item message", `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file-private"}]}}`},
			{"session input", `{"type":"session.update","session":{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}}`},
			{"session content", `{"type":"session.update","session":{"content":[{"type":"input_file","file_id":"file-private"}]}}`},
			{"nested response", `{"type":"response.create","response":{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-private"}]}]}}`},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{managedFilesEnabled: true, firstPayload: `{"type":"response.create","model":"gpt-5.4","input":"hello"}`, secondPayload: tc.payload, ingressMode: mode.ingress, rawRelay: mode.raw, secondTurnCloseExpected: true, closeReason: "Managed files require HTTP Responses"})
			})
		}
	}
}

func TestManagedFilesWebSocketPreservesInlineAndOpaqueContent(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode, func(t *testing.T) {
			runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{managedFilesEnabled: true, ingressMode: mode, firstPayload: `{"type":"response.create","model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="},{"type":"input_text","text":"{\"type\":\"input_file\",\"file_id\":\"file-private\"}"}]}]}`, secondPayload: `{"type":"response.create","input":"hello","tools":[{"type":"function","name":"opaque","parameters":{"type":"object","properties":{"file_id":{"type":"string"}}}}]}`})
		})
	}
}
