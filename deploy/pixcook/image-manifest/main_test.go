package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestMatchesTrimSpace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001_test.sql"), []byte("\u2003 \r\nSELECT 1;\t\n\u00a0"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.go"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := buildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte("SELECT 1;")))
	if len(got) != 1 || got["001_test.sql"] != want {
		t.Fatalf("manifest = %#v, want SQL checksum %s", got, want)
	}
}

func TestManifestRejectsMissingOrEmptyInput(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "absent")} {
		if _, err := buildManifest(dir); err == nil {
			t.Fatalf("accepted empty input %s", dir)
		}
	}
}
