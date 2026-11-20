package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type storedForwardCaptureDB struct{ captureTestDB }

func (d *storedForwardCaptureDB) GetForOwner(_ context.Context, userID, groupID int64, _ string) (*StoredResponse, error) {
	if userID != 1 || groupID != 3 || d.terminal == nil {
		return nil, ErrStoredResponseNotFound
	}
	return d.terminal, nil
}

func TestStoredResponseForwardJSONMatchesRetrievedPublicResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &storedForwardCaptureDB{}
	stored, err := NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NormalizeStoredResponseRequest([]byte(`{"model":"client-model","input":"hello","instructions":"client instructions"}`))
	if err != nil {
		t.Fatal(err)
	}
	owner := StoredResponseOwner{1, 2, 3}
	capture, err := stored.Begin(context.Background(), owner, request, StoredResponseBinding{AccountID: 4, ResolvedModel: "native-model", Scope: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cleanup := withStoredResponseCapture(context.Background(), capture)
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	payload := `{"id":"resp_native","object":"response","status":"completed","model":"native-model","instructions":"private upstream instructions","output":[{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}`
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(payload))}
	account := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	result, err := (&OpenAIGatewayService{}).handleNonStreamingResponse(ctx, response, c, account, "client-model", "native-model")
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		ID, Model, Instructions string
		Store                   bool
	}
	if err = json.Unmarshal(recorder.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if public.ID != capture.PublicID() || public.Model != "client-model" || public.Instructions != "client instructions" || !public.Store {
		t.Fatalf("response bypassed local capture: %s", recorder.Body.String())
	}
	if result == nil || result.responseID != public.ID || result.usage.InputTokens != 7 || result.usage.OutputTokens != 2 {
		t.Fatal("forward metadata lost local identity or usage")
	}
	saved, err := stored.Get(context.Background(), owner, public.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved.PublicResponse, recorder.Body.Bytes()) {
		t.Fatal("retrieved response differs from delivered JSON")
	}
}

func storedForwardFixture(t *testing.T, fail bool) (*StoredResponseService, *StoredResponseCapture, context.Context, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &storedForwardCaptureDB{}
	if fail {
		db.finishErr = errors.New("private database unavailable")
	}
	svc, err := NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := NormalizeStoredResponseRequest([]byte(`{"model":"client-model","input":"hello","instructions":"client instructions"}`))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, ResolvedModel: "native-model", Scope: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cleanup := withStoredResponseCapture(context.Background(), capture)
	t.Cleanup(func() {
		db.finishErr = nil
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	return svc, capture, ctx, c, recorder
}

func TestStoredResponseForwardSSEIdentityAndCommitBoundary(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "finalize_failure"}[fail], func(t *testing.T) {
			svc, capture, ctx, c, recorder := storedForwardFixture(t, fail)
			raw := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"native-model\",\"output\":[]}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_native\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"hello\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"native-model\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9}}}\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(raw))}
			account := newOpenAIRejectedFieldTestAccount()
			result, err := newOpenAIRejectedFieldTestService(nil).handleStreamingResponse(ctx, resp, c, account, time.Now(), "client-model", "native-model")
			if result == nil || result.responseID != capture.PublicID() || result.usage.InputTokens != 7 || result.usage.OutputTokens != 2 {
				t.Fatalf("identity/usage lost: %#v %v", result, err)
			}
			if strings.Contains(recorder.Body.String(), "resp_native") {
				t.Fatalf("native ID leaked: %s", recorder.Body.String())
			}
			if fail {
				var local *StoredResponseLocalError
				if !errors.As(err, &local) {
					t.Fatalf("storage error classification: %v", err)
				}
				if strings.Contains(recorder.Body.String(), "response.completed") || !strings.Contains(recorder.Body.String(), "response.failed") {
					t.Fatalf("terminal escaped commit: %s", recorder.Body.String())
				}
				if !strings.Contains(recorder.Body.String(), capture.PublicID()) || strings.Contains(recorder.Body.String(), "private database") {
					t.Fatal("unsafe local error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := svc.Get(ctx, StoredResponseOwner{1, 2, 3}, capture.PublicID())
			if err != nil {
				t.Fatal(err)
			}
			_, terminal, ok := extractOpenAISSETerminalEvent(recorder.Body.String())
			if !ok {
				t.Fatal("missing terminal")
			}
			if !bytes.Equal(saved.PublicResponse, []byte(gjson.GetBytes(terminal, "response").Raw)) {
				t.Fatalf("retrieve mismatch: %s %s", saved.PublicResponse, terminal)
			}
		})
	}
}

func TestStoredResponseForwardSSEToJSONCommitBoundary(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "storage_failure"}[fail], func(t *testing.T) {
			svc, capture, ctx, c, recorder := storedForwardFixture(t, fail)
			payload := "{\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"id\":\"resp_native\",\"status\":\"completed\",\"model\":\"native-model\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + payload + "\n\n"))}
			result, err := newOpenAIRejectedFieldTestService(nil).handleNonStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), "client-model", "native-model")
			if result == nil || result.responseID != capture.PublicID() || result.usage.InputTokens != 9 || result.usage.OutputTokens != 4 {
				t.Fatalf("usage/identity lost: %#v %v", result, err)
			}
			if fail {
				var local *StoredResponseLocalError
				if !errors.As(err, &local) || recorder.Code != 503 || strings.Contains(recorder.Body.String(), "completed") {
					t.Fatalf("storage failure escaped: %d %s %v", recorder.Code, recorder.Body.String(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := svc.Get(ctx, StoredResponseOwner{1, 2, 3}, capture.PublicID())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(saved.PublicResponse, recorder.Body.Bytes()) {
				t.Fatal("retrieve mismatch")
			}
		})
	}
}

func TestStoredResponseForwardSafeFailureUsageAndStorageFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream_%t_storagefailure_%t", stream, fail), func(t *testing.T) {
				svc, capture, ctx, c, recorder := storedForwardFixture(t, fail)
				failure := map[string]any{"type": "server_error", "code": "basispoints_protocol_error", "message": "Basispoints returned an invalid response stream.", "failure_stage": "bridge_protocol_conversion"}
				payload, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_native", "status": "failed", "output": []any{}, "error": failure, "usage": map[string]any{"input_tokens": 9, "output_tokens": 4, "private": "hidden"}}})
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Bps-Response-Contract": []string{"failure-v1"}}, Body: io.NopCloser(strings.NewReader("event: response.failed\ndata: " + string(payload) + "\n\n"))}
				service := newOpenAIRejectedFieldTestService(nil)
				var usage *OpenAIUsage
				var id string
				var err error
				if stream {
					var result *openaiStreamingResult
					result, err = service.handleStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), time.Now(), "client-model", "native-model")
					if result != nil {
						usage, id = result.usage, result.responseID
					}
				} else {
					var result *openaiNonStreamingResult
					result, err = service.handleNonStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), "client-model", "native-model")
					if result != nil {
						usage, id = result.usage, result.responseID
					}
				}
				if err == nil || usage == nil || usage.InputTokens != 9 || usage.OutputTokens != 4 || id != capture.PublicID() {
					t.Fatalf("failure usage/identity lost: %s %#v %v", id, usage, err)
				}
				if strings.Contains(recorder.Body.String(), "hidden") || strings.Contains(recorder.Body.String(), "failure_stage") || strings.Contains(recorder.Body.String(), "resp_native") {
					t.Fatalf("private diagnostic leaked: %s", recorder.Body.String())
				}
				if fail {
					var local *StoredResponseLocalError
					if !errors.As(err, &local) || recorder.Code != 503 {
						t.Fatalf("storage failure classification/status: %d %v", recorder.Code, err)
					}
					return
				}
				if !stream && recorder.Code != 502 {
					t.Fatalf("failed response became success: %d %s", recorder.Code, recorder.Body.String())
				}
				saved, getErr := svc.Get(ctx, StoredResponseOwner{1, 2, 3}, capture.PublicID())
				if getErr != nil {
					t.Fatal(getErr)
				}
				written := recorder.Body.Bytes()
				if stream {
					_, written, _ = extractOpenAISSETerminalEvent(string(written))
				}
				written = []byte(gjson.GetBytes(written, "response").Raw)
				if !bytes.Equal(saved.PublicResponse, written) {
					t.Fatalf("retrieved safe failure mismatch: %s %s", saved.PublicResponse, written)
				}
			})
		}
	}
}

func TestStoredResponseForwardSSEToJSONRequiresTerminal(t *testing.T) {
	_, _, ctx, c, recorder := storedForwardFixture(t, false)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"object\":\"response\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))}
	_, err := newOpenAIRejectedFieldTestService(nil).handleNonStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), "client-model", "native-model")
	if err == nil || recorder.Code != 503 || strings.Contains(recorder.Body.String(), "in_progress") {
		t.Fatalf("unfinished SSE escaped: %d %s %v", recorder.Code, recorder.Body.String(), err)
	}
}

func TestStoredResponseForwardPrivatePartialAndKeepalive(t *testing.T) {
	for _, event := range []string{"response.output_text.delta", "response.content_part.added", "keepalive"} {
		t.Run(event, func(t *testing.T) {
			_, _, ctx, c, recorder := storedForwardFixture(t, false)
			payload := map[string]any{"type": event, "output_index": 0, "content_index": 0, "item_id": "msg_1", "delta": "ok", "native_replay": map[string]any{"secret": "private-secret"}}
			if event == "response.content_part.added" {
				delete(payload, "delta")
				delete(payload, "native_replay")
				payload["part"] = map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "native_replay": "private-secret"}
			}
			raw, _ := json.Marshal(payload)
			body := "event: " + event + "\ndata: " + string(raw) + "\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
			_, err := newOpenAIRejectedFieldTestService(nil).handleStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), time.Now(), "client-model", "native-model")
			if strings.Contains(recorder.Body.String(), "private-secret") {
				t.Fatalf("private event escaped: %s", recorder.Body.String())
			}
			if event != "keepalive" && err == nil {
				t.Fatal("private event accepted")
			}
		})
	}
}

func TestStoredResponseForwardBufferedProgressSurvivesSparseFailureOrEOF(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			svc, capture, ctx, c, recorder := storedForwardFixture(t, false)
			body := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"object\":\"response\",\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"partial\"}\n\n"
			if terminal {
				body += "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"code\":\"basispoints_protocol_error\",\"message\":\"Basispoints returned an invalid response stream.\"},\"usage\":{\"input_tokens\":10}}}\n\n"
			}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Bps-Response-Contract": []string{"failure-v1"}}, Body: io.NopCloser(strings.NewReader(body))}
			result, err := newOpenAIRejectedFieldTestService(nil).handleNonStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), "client-model", "native-model")
			if err == nil || result == nil || result.usage.OutputTokens != 4 || result.usage.InputTokens != map[bool]int{false: 9, true: 10}[terminal] {
				t.Fatalf("buffered usage lost: %#v %s %v", result, recorder.Body.String(), err)
			}
			if !terminal {
				_ = storedResponseAttemptFrom(ctx).stopLease()
				if err = capture.Abort(ctx); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := svc.Get(ctx, StoredResponseOwner{1, 2, 3}, capture.PublicID())
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(saved.PublicResponse, "usage.output_tokens").Int() != 4 {
				t.Fatalf("stored usage lost: %s", saved.PublicResponse)
			}
			if !terminal && !strings.Contains(string(saved.PublicResponse), "partial") {
				t.Fatalf("partial lost: %s", saved.PublicResponse)
			}
		})
	}
}

func TestStoredResponseForwardMismatchedInvalidFailureDoesNotSettle(t *testing.T) {
	_, _, ctx, c, recorder := storedForwardFixture(t, false)
	body := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"object\":\"response\",\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"code\":\"basispoints_protocol_error\",\"message\":\"private raw failure\"},\"usage\":{\"input_tokens\":100,\"output_tokens\":100}}}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Bps-Response-Contract": []string{"failure-v1"}}, Body: io.NopCloser(strings.NewReader(body))}
	result, err := newOpenAIRejectedFieldTestService(nil).handleNonStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), "client-model", "native-model")
	if err == nil || result == nil || result.usage.InputTokens != 9 || result.usage.OutputTokens != 4 || strings.Contains(recorder.Body.String(), "private raw") {
		t.Fatalf("untrusted current usage settled: %#v %s %v", result, recorder.Body.String(), err)
	}
}

type storedCaptureReadError struct{}

func (storedCaptureReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestStoredResponseForwardBufferedReadErrorRetainsProgress(t *testing.T) {
	svc, capture, ctx, c, recorder := storedForwardFixture(t, false)
	prefix := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"object\":\"response\",\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"partial\"}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(io.MultiReader(strings.NewReader(prefix), storedCaptureReadError{}))}
	result, err := newOpenAIRejectedFieldTestService(nil).handleNonStreamingResponse(ctx, resp, c, newOpenAIRejectedFieldTestAccount(), "client-model", "native-model")
	if err == nil || result == nil || result.usage.InputTokens != 9 || result.usage.OutputTokens != 4 || recorder.Code != 503 {
		t.Fatalf("read failure lost usage: %#v %d %v", result, recorder.Code, err)
	}
	_ = storedResponseAttemptFrom(ctx).stopLease()
	if err = capture.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := svc.Get(ctx, StoredResponseOwner{1, 2, 3}, capture.PublicID())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved.PublicResponse), "partial") || gjson.GetBytes(saved.PublicResponse, "usage.output_tokens").Int() != 4 {
		t.Fatalf("read failure lost partial: %s", saved.PublicResponse)
	}
}
