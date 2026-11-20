package upstreamrecord

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestHeadersRedactsManagedFileProof(t *testing.T) {
	header := http.Header{}
	for _, key := range []string{"X-Bps-Service-Key", "X-Bps-File-Admission", "X-Bps-File-Binding"} {
		header.Set(key, "private-proof")
	}
	redacted := Headers(header)
	for _, key := range []string{"X-Bps-Service-Key", "X-Bps-File-Admission", "X-Bps-File-Binding"} {
		require.Equal(t, "[redacted]", redacted.Get(key))
	}
}
