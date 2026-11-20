//go:build unit

package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCollectManagedResponseFilesTypedBoundary(t *testing.T) {
	refs, err := CollectManagedResponseFiles([]byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-local1"},{"type":"input_file","file_id":"file-local1"} ]},{"type":"function_call","arguments":"{\"file_id\":\"file-opaque\"}"}]}`))
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "file-local1", refs[0].PublicID)
	require.Len(t, refs[0].Positions, 2)
}

func TestCollectManagedResponseFilesRejectAmbiguousShapes(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"role":"user","content":[{"type":"input_file","type":"input_text","file_id":"file-a"}]}]}`,
		`{"input":[{"type":"function_call_output","output":[{"type":"tool_result","content":[{"type":"input_file","file_id":"file-a"}]}]}]}`,
		`{"input":[],"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`,
		`{"Input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","File_ID":"file-a"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a","file_url":"https://example.test/a"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a","file_id":"file-b"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":null}]}]}`,
		`{"input":[{"type":"function_call_output","output":[{"type":"input_file","file_id":"file-a"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-a"}]}]}`,
		`{"input":{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			_, err := CollectManagedResponseFiles([]byte(body))
			require.ErrorIs(t, err, ErrManagedFileUnsupportedInput)
		})
	}
}

func TestCollectManagedResponseFilesPreservesInlineAndOpaqueInputs(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_data":"data:text/plain;base64,AA=="}]}]}`,
		`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_file\",\"file_id\":\"file-x\"}"}]}`,
	} {
		refs, err := CollectManagedResponseFiles([]byte(body))
		require.NoError(t, err)
		require.Empty(t, refs)
	}
}
