//go:build unit

package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type managedErrorTerminalUpstream struct {
	service.HTTPUpstream
	status       int
	beforeReturn func()
	calls        int
}

func (u *managedErrorTerminalUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.beforeReturn()
	return &http.Response{
		StatusCode: u.status,
		Header:     http.Header{"Content-Type": {"application/json"}, "Retry-After": {"7"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"upstream unavailable"}}`)),
	}, nil
}

// The trusted plan is attached after the upstream call starts, isolating the
// handler terminal boundary from file resolution and scheduler fixtures.
func TestManagedFileResponsesErrorPreservesHTTPCommitState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name               string
		stream, committed  bool
		status, wantStatus int
		wantType           string
	}{
		{"nonstream rate limit", false, false, 429, 429, "rate_limit_error"},
		{"stream rate limit before commit", true, false, 429, 429, "rate_limit_error"},
		{"nonstream server failure", false, false, 503, 502, "upstream_error"},
		{"stream server failure before commit", true, false, 503, 502, "upstream_error"},
		{"stream rate limit after commit", true, true, 429, 200, "rate_limit_error"},
		{"stream server failure after commit", true, true, 503, 200, "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &managedErrorTerminalUpstream{status: tc.status}
			h := newOpenAIResponsesFailoverTestHandler(t, u)
			c, rec := newOpenAIResponsesFailoverTestContext(t, nil)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello"}`, tc.stream)))
			c.Request.Header.Set("Content-Type", "application/json")
			u.beforeReturn = func() {
				c.Request = c.Request.WithContext(service.WithManagedFilePlan(c.Request.Context(), &service.ManagedFilePlan{}))
				if tc.committed {
					c.Header("Content-Type", "text/event-stream")
					_, _ = io.WriteString(c.Writer, ": keepalive\n\n")
					c.Writer.Flush()
				}
			}

			h.Responses(c)

			require.Equal(t, 1, u.calls, "managed inference must never replay")
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tc.committed {
				body := rec.Body.String()
				require.Contains(t, body, ": keepalive\n\n")
				require.Equal(t, 1, strings.Count(body, "event: response.failed\n"))
				data := body[strings.Index(body, "data: ")+len("data: "):]
				wantCode := "upstream_error"
				if tc.status == http.StatusTooManyRequests {
					wantCode = "rate_limit_exceeded"
				}
				require.Equal(t, wantCode, gjson.Get(data, "response.error.code").String())
			} else {
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
				require.Equal(t, tc.wantType, gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
				require.NotContains(t, rec.Body.String(), "event:")
				require.Equal(t, "7", rec.Header().Get("Retry-After"))
			}
		})
	}
}
