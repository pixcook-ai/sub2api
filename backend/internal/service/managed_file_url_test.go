package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
)

func TestManagedFileURLStagesExactBytesAtOriginalPosition(t *testing.T) {
	var dialed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "files.example" || r.URL.RawQuery != "signature=private-value" {
			t.Errorf("target path/authority changed: %s", r.Host)
		}
		for _, key := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Bps-Files-Key"} {
			if r.Header.Get(key) != "" {
				t.Errorf("credential forwarded: %s", key)
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello")
	}))
	defer server.Close()
	network := managedFileURLNetwork{
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return new(net.Dialer).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	parent := t.TempDir()
	position := managedfiles.Position{Input: 2, Part: 3}
	stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/hello.txt?signature=private-value", Position: position}}, nil, DefaultManagedInlineLimits(), parent, network)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stage.Close() }()
	files := stage.Files()
	if len(files) != 1 || files[0].Filename != "hello.txt" || files[0].MIME != "text/plain" || files[0].Bytes != 5 || files[0].PublicID != "" || files[0].Position != position || files[0].SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("wrong staged metadata: %+v", files)
	}
	if len(dialed) != 1 || dialed[0] != "93.184.216.34:80" {
		t.Fatalf("DNS was not pinned: %v", dialed)
	}
	r, err := stage.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(data) != "hello" {
		t.Fatalf("wrong body %q: %v", data, err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging not removed: %v %v", entries, err)
	}
	if _, err := stage.Open(0); err == nil {
		t.Fatal("closed stage reopened")
	}
	if strings.Contains(files[0].Filename, "private-value") {
		t.Fatal("URL query entered metadata")
	}
}

func managedFileURLTestNetwork(server *httptest.Server, lookups func(context.Context, string) ([]netip.Addr, error), calls *atomic.Int32) managedFileURLNetwork {
	if lookups == nil {
		lookups = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
	}
	return managedFileURLNetwork{lookup: lookups, dial: func(ctx context.Context, protocol, address string) (net.Conn, error) {
		if calls != nil {
			calls.Add(1)
		}
		return new(net.Dialer).DialContext(ctx, protocol, server.Listener.Addr().String())
	}}
}

func TestManagedFileURLRejectsPartialAndAmbiguousResponses(t *testing.T) {
	for _, kind := range []string{"partial", "ambiguous redirect"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if kind == "ambiguous redirect" && r.URL.Path != "/hello.txt" {
					w.Header().Add("Location", "/hello.txt")
					w.Header().Add("Location", "http://127.0.0.1/secret")
					w.WriteHeader(http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				if kind == "partial" {
					w.Header().Set("Content-Range", "bytes 0-4/100")
				}
				_, _ = io.WriteString(w, "hello")
			}))
			defer server.Close()
			parent := t.TempDir()
			stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/start.txt"}}, nil, DefaultManagedInlineLimits(), parent, managedFileURLTestNetwork(server, nil, nil))
			if stage != nil {
				_ = stage.Close()
			}
			if err == nil {
				t.Fatal("partial or ambiguous response was accepted")
			}
			if requests.Load() != 1 {
				t.Fatalf("ambiguous redirect was followed: %d", requests.Load())
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatalf("failed download leaked %d entries", len(entries))
			}
		})
	}
}

func TestManagedFileURLRejectsUnsafeTargetsBeforeNetwork(t *testing.T) {
	for _, target := range []string{
		"file:///etc/passwd", "ftp://files.example/a.txt", "https://secret:token@files.example/a.txt",
		"http://localhost/a.txt", "http://LOCALHOST./a.txt", "http://metadata.google.internal/a.txt",
		"http://127.0.0.1/a.txt", "http://10.0.0.1/a.txt", "http://172.16.1.1/a.txt", "http://192.168.1.1/a.txt",
		"http://169.254.169.254/a.txt", "http://100.100.100.200/a.txt", "http://0.1.2.3/a.txt", "http://224.0.0.1/a.txt",
		"http://192.0.2.1/a.txt", "http://198.18.0.1/a.txt", "http://203.0.113.1/a.txt", "http://240.0.0.1/a.txt",
		"http://192.31.196.1/a.txt", "http://192.52.193.1/a.txt", "http://192.175.48.1/a.txt",
		"http://[::1]/a.txt", "http://[::ffff:127.0.0.1]/a.txt", "http://[fd00::1]/a.txt", "http://[fe80::1%25en0]/a.txt",
		"http://[2001:db8::1]/a.txt", "http://[64:ff9b::7f00:1]/a.txt", "http://[2002:7f00:1::1]/a.txt",
		"http://[2620:4f:8000::1]/a.txt", "http://[3fff::1]/a.txt",
		"https://files.example:99999/a.txt", "https://files.example/a.txt#fragment", " https://files.example/a.txt",
	} {
		t.Run(target, func(t *testing.T) {
			network := managedFileURLNetwork{lookup: func(context.Context, string) ([]netip.Addr, error) {
				t.Error("unsafe static target reached DNS")
				return nil, errors.New("forbidden test lookup")
			}}
			stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/valid.txt"}, {URL: target, Position: managedfiles.Position{Input: 1}}}, nil, DefaultManagedInlineLimits(), t.TempDir(), network)
			if stage != nil {
				_ = stage.Close()
			}
			if err == nil {
				t.Fatal("unsafe URL accepted")
			}
		})
	}
}

func TestManagedFileURLRejectsAnyUnsafeDNSAddressWithoutDial(t *testing.T) {
	for name, addresses := range map[string][]netip.Addr{
		"private": {netip.MustParseAddr("127.0.0.1")},
		"mixed":   {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")},
		"mapped":  {netip.MustParseAddr("::ffff:169.254.169.254")},
		"empty":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			network := managedFileURLNetwork{lookup: func(context.Context, string) ([]netip.Addr, error) { return addresses, nil }, dial: func(context.Context, string, string) (net.Conn, error) {
				t.Error("unsafe DNS resolution reached dial")
				return nil, errors.New("forbidden test dial")
			}}
			stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/a.txt?token=secret-query"}}, nil, DefaultManagedInlineLimits(), t.TempDir(), network)
			if stage != nil {
				_ = stage.Close()
			}
			var rejected *managedfiles.Error
			if !errors.As(err, &rejected) || rejected.Code != "unsafe_file_url" || strings.Contains(err.Error(), "secret-query") {
				t.Fatalf("wrong safe rejection: %v", err)
			}
		})
	}
}

func TestManagedFileURLRevalidatesRedirectDNSAndDisablesAmbientCredentials(t *testing.T) {
	for _, rebind := range []bool{false, true} {
		t.Run(map[bool]string{true: "rebinding rejected", false: "public redirect"}[rebind], func(t *testing.T) {
			var lookups, requests, dials atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("ambient proxy was used"); w.WriteHeader(500) }))
			defer proxy.Close()
			t.Setenv("HTTP_PROXY", proxy.URL)
			t.Setenv("HTTPS_PROXY", proxy.URL)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				for _, header := range []string{"Authorization", "Cookie", "Referer", "Proxy-Authorization"} {
					if r.Header.Get(header) != "" {
						t.Errorf("redirect forwarded %s", header)
					}
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("compression not disabled")
				}
				if r.URL.Path == "/first.txt" {
					w.Header().Set("Set-Cookie", "session=should-not-follow")
					w.Header().Set("Location", "/second.txt")
					w.WriteHeader(http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "hello")
			}))
			defer server.Close()
			network := managedFileURLTestNetwork(server, func(context.Context, string) ([]netip.Addr, error) {
				if lookups.Add(1) > 1 && rebind {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}, &dials)
			stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/first.txt?token=private-query"}}, nil, DefaultManagedInlineLimits(), t.TempDir(), network)
			if stage != nil {
				_ = stage.Close()
			}
			if rebind {
				if err == nil || requests.Load() != 1 || dials.Load() != 1 {
					t.Fatalf("rebind reached private target: err=%v requests=%d dials=%d", err, requests.Load(), dials.Load())
				}
			} else if err != nil || requests.Load() != 2 || dials.Load() != 2 {
				t.Fatalf("public redirect failed: %v", err)
			}
			if lookups.Load() != 2 {
				t.Fatalf("redirect DNS was reused: %d", lookups.Load())
			}
		})
	}
}

func TestManagedFileURLCancellationAndTruncatedTransferCleanup(t *testing.T) {
	for _, mode := range []string{"cancel", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				if mode == "truncated" {
					w.Header().Set("Content-Length", "100")
					_, _ = io.WriteString(w, "short")
					return
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				go func() { <-started; cancel() }()
			}
			parent := t.TempDir()
			stage, err := stageManagedFileURLs(ctx, []ManagedFileURLSource{{URL: "http://files.example/a.txt?private=secret-query"}}, nil, DefaultManagedInlineLimits(), parent, managedFileURLTestNetwork(server, nil, nil))
			if stage != nil {
				_ = stage.Close()
			}
			if err == nil || strings.Contains(err.Error(), "secret-query") {
				t.Fatalf("unsafe error: %v", err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("failed request leaked bytes")
			}
		})
	}
}

func TestManagedFileURLCallerDeadlineBoundsHeaderWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	stage, err := stageManagedFileURLs(ctx, []ManagedFileURLSource{{URL: "http://files.example/a.txt"}}, nil, DefaultManagedInlineLimits(), t.TempDir(), managedFileURLTestNetwork(server, nil, nil))
	if stage != nil {
		_ = stage.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline not preserved: %v", err)
	}
}

func TestManagedFileURLStreamsUnknownLengthWithinSharedBudgets(t *testing.T) {
	for _, kind := range []string{"exact", "single overflow", "shared message", "shared request", "earlier URL"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush() // no Content-Length, including for five bytes
				_, _ = io.WriteString(w, "hello")
			}))
			defer server.Close()
			limits := DefaultManagedInlineLimits()
			limits.FileBytes, limits.MessageBytes, limits.TotalBytes = 5, 10, 10
			sources := []ManagedFileURLSource{{URL: "http://files.example/a.txt", Position: managedfiles.Position{Part: 1}}}
			var existing []ManagedInlineFileDescriptor
			switch kind {
			case "single overflow":
				limits.FileBytes = 4
			case "shared message":
				existing = []ManagedInlineFileDescriptor{{PublicID: "file-public", Bytes: 4}}
				limits.MessageBytes = 8
			case "shared request":
				existing = []ManagedInlineFileDescriptor{{Bytes: 4, Position: managedfiles.Position{Input: 1}}}
				limits.TotalBytes = 8
			case "earlier URL":
				sources = append(sources, ManagedFileURLSource{URL: "http://files.example/b.txt", Position: managedfiles.Position{Part: 2}})
				limits.TotalBytes = 8
			}
			parent := t.TempDir()
			stage, err := stageManagedFileURLs(context.Background(), sources, existing, limits, parent, managedFileURLTestNetwork(server, nil, nil))
			if stage != nil {
				defer func() { _ = stage.Close() }()
			}
			if kind == "exact" {
				if err != nil || stage == nil || stage.Files()[0].Bytes != 5 {
					t.Fatalf("exact unknown-length file failed: %v", err)
				}
				return
			}
			var rejected *managedfiles.Error
			if !errors.As(err, &rejected) || stage != nil {
				t.Fatalf("stream budget bypassed: stage=%v error=%v", stage, err)
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("overflow leaked earlier or partial bytes")
			}
			want := int32(1)
			if kind == "earlier URL" {
				want = 2
			}
			if requests.Load() != want {
				t.Fatalf("unexpected fetch count: %d", requests.Load())
			}
		})
	}
}

func TestManagedFileURLValidatesDownloadedContentAndTypes(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9ZlS8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, filename, mime string
		data                 []byte
		valid                bool
	}{
		{"PNG", "pixel.png", "image/png", pngBytes, true},
		{"PDF", "doc.pdf", "application/pdf", []byte("%PDF-1.7\nfixture"), true},
		{"binary executable", "doc.txt", "text/plain", []byte("MZbinary"), false},
		{"ZIP disguised", "doc.txt", "text/plain", []byte("PK\x03\x04file"), false},
		{"PDF mismatch", "doc.pdf", "application/pdf", []byte("hello"), false},
		{"image mismatch", "pixel.png", "image/png", []byte("hello"), false},
		{"image MIME mismatch", "pixel.jpg", "image/jpeg", pngBytes, false},
		{"malformed MIME", "doc.txt", "text/plain;bad=", []byte("hello"), false},
		{"unsupported binary", "file.exe", "application/x-msdownload", []byte("hello"), false},
		{"empty", "doc.txt", "text/plain", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.mime)
				_, _ = w.Write(tc.data)
			}))
			defer server.Close()
			parent := t.TempDir()
			stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/download", Filename: tc.filename}}, nil, DefaultManagedInlineLimits(), parent, managedFileURLTestNetwork(server, nil, nil))
			if stage != nil {
				defer func() { _ = stage.Close() }()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if !tc.valid {
				entries, _ := os.ReadDir(parent)
				if len(entries) != 0 {
					t.Fatal("invalid content leaked staging")
				}
				return
			}
			r, err := stage.Open(0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, tc.data) {
				t.Fatalf("content changed: %v", err)
			}
		})
	}
}

func TestManagedFileURLRejectsUnsafeRedirectAndUnsupportedResponseBeforeDisk(t *testing.T) {
	for _, kind := range []string{"loopback", "metadata", "redirect loop", "compressed", "status", "duplicate MIME", "declared overflow"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				switch kind {
				case "loopback", "metadata", "redirect loop":
					location := "/again.txt"
					if kind == "loopback" {
						location = "http://127.0.0.1/secret"
					}
					if kind == "metadata" {
						location = "http://169.254.169.254/secret"
					}
					w.Header().Set("Location", location)
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				case "compressed":
					w.Header().Set("Content-Encoding", "gzip")
				case "status":
					w.WriteHeader(http.StatusForbidden)
					return
				case "duplicate MIME":
					w.Header().Add("Content-Type", "application/pdf")
				case "declared overflow":
					w.Header().Set("Content-Length", "100")
				}
				_, _ = io.WriteString(w, "hello")
			}))
			defer server.Close()
			parent := t.TempDir()
			limits := DefaultManagedInlineLimits()
			limits.FileBytes = 5
			stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/a.txt?secret=private-query"}}, nil, limits, parent, managedFileURLTestNetwork(server, nil, nil))
			if stage != nil {
				_ = stage.Close()
			}
			var rejected *managedfiles.Error
			if !errors.As(err, &rejected) || strings.Contains(err.Error(), "private-query") {
				t.Fatalf("unsafe response accepted or URL leaked: %v", err)
			}
			want := int32(1)
			if kind == "redirect loop" {
				want = 4
			}
			if requests.Load() != want {
				t.Fatalf("redirect policy bypassed: %d", requests.Load())
			}
			entries, _ := os.ReadDir(parent)
			if len(entries) != 0 {
				t.Fatal("response rejected after persistent disk allocation")
			}
		})
	}
}

func TestManagedFileURLDefaultLimitRejectsLargeChunkedTransferAndCleans(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		chunk := bytes.Repeat([]byte{97}, 1<<20)
		for i := int64(0); i <= 200; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	parent := t.TempDir()
	stage, err := stageManagedFileURLs(context.Background(), []ManagedFileURLSource{{URL: "http://files.example/large.txt"}}, nil, DefaultManagedInlineLimits(), parent, managedFileURLTestNetwork(server, nil, nil))
	if stage != nil {
		_ = stage.Close()
	}
	var rejected *managedfiles.Error
	if !errors.As(err, &rejected) || rejected.Code != "managed_file_limit" {
		t.Fatalf("200 MiB limit bypassed: %v", err)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 0 {
		t.Fatal("large partial download leaked")
	}
}

func TestManagedFileURLStorageFailurePreservesErrnoAndCleansEarlierBytes(t *testing.T) {
	for _, errno := range []error{syscall.ENOSPC, syscall.EIO, io.ErrShortWrite} {
		t.Run(errno.Error(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "hello")
			}))
			defer server.Close()
			fs := defaultManagedInlineStagingFilesystem()
			create := fs.create
			created := 0
			fs.create = func(root *os.Root, name string) (io.WriteCloser, error) {
				file, err := create(root, name)
				if err != nil {
					return nil, err
				}
				created++
				if created == 2 {
					return &managedFileURLWriteFailure{WriteCloser: file, err: errno}, nil
				}
				return file, nil
			}
			parent := t.TempDir()
			sources := []ManagedFileURLSource{{URL: "http://files.example/first.txt?token=secret-query"}, {URL: "http://files.example/second.txt", Position: managedfiles.Position{Part: 1}}}
			stage, err := stageManagedFileURLsWithFilesystem(context.Background(), sources, nil, DefaultManagedInlineLimits(), parent, managedFileURLTestNetwork(server, nil, nil), fs)
			if stage != nil {
				_ = stage.Close()
			}
			var storage *managedInlineStorageError
			if stage != nil || created != 2 || !errors.As(err, &storage) || !errors.Is(err, errno) {
				t.Fatalf("storage failure lost: stage=%v creates=%d err=%v", stage, created, err)
			}
			if err.Error() != "inline file staging failed" || strings.Contains(err.Error(), "secret-query") || strings.Contains(err.Error(), parent) {
				t.Fatalf("unsafe storage error: %v", err)
			}
			entries, readErr := os.ReadDir(parent)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("storage failure leaked bytes: %v %v", entries, readErr)
			}
		})
	}
}

type managedFileURLWriteFailure struct {
	io.WriteCloser
	err error
}

func (w *managedFileURLWriteFailure) Write(data []byte) (int, error) {
	if len(data) > 2 {
		data = data[:2]
	}
	n, err := w.WriteCloser.Write(data)
	if err != nil {
		return n, err
	}
	if w.err == io.ErrShortWrite {
		return n, nil
	}
	return n, w.err
}
