//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

func TestManagedFileURLPreflightAcceptsCanonicalSourceWithoutFetching(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://files.example/hello.txt?signature=private-value"}]}]}`)
	plan, err := PreflightManagedInlineFiles(context.Background(), body, DefaultManagedInlineLimits())
	require.NoError(t, err)
	require.Equal(t, []ManagedInlineFileDescriptor{{Filename: "hello.txt", Position: managedfiles.Position{Input: 0, Part: 0}}}, plan.Files())
	checked, err := plan.WithPublicFiles(nil)
	require.NoError(t, err)
	require.True(t, checked.ReadyForStaging())
}

func managedURLIntegrationDownloader(resolver *ManagedFileResolver, server *httptest.Server, calls *atomic.Int32) {
	resolver.stageURLs = func(ctx context.Context, sources []ManagedFileURLSource, existing []ManagedInlineFileDescriptor, limits ManagedInlineLimits, parent string) (*ManagedInlineStaging, error) {
		return stageManagedFileURLs(ctx, sources, existing, limits, parent, managedFileURLTestNetwork(server, nil, calls))
	}
}

func TestManagedFileURLResolverValidatesEntireBatchBeforeAnyFetchOrOpen(t *testing.T) {
	const prefix = `{"input":[{"role":"user","content":[{"type":"input_file","file_url":"http://files.example/hello.txt"},`
	for name, body := range map[string]string{
		"invalid later inline":   prefix + `{"type":"input_file","filename":"late.txt","file_data":"!bad"}]}]}`,
		"foreign later public":   prefix + `{"type":"input_file","file_id":"file-foreign"}]}]}`,
		"unsafe later URL":       prefix + `{"type":"input_file","file_url":"http://127.0.0.1/a.txt"}]}]}`,
		"source ambiguity":       prefix + `{"type":"input_file","file_id":"file-public","file_url":"http://files.example/b.txt"}]}]}`,
		"source alias":           prefix + `{"type":"input_file","File_URL":"http://files.example/b.txt"}]}]}`,
		"decoded duplicate key":  prefix + `{"type":"input_file","file_url":"http://files.example/b.txt","file_\u0075rl":"http://files.example/c.txt"}]}]}`,
		"noncanonical container": prefix + `{"type":"tool_result","content":[{"type":"input_file","file_url":"http://files.example/b.txt"}]}]}]}`,
		"invalid URL filename":   prefix + `{"type":"input_file","file_url":"http://files.example/b.txt","filename":"../b.txt"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-public": managedResolverRecord("file-public")}}
			files := managedResolverFiles(t, repo)
			ledger := &managedResolverLedger{}
			resolver := NewManagedFileResolver(files, ledger, nil, nil)
			var downloads atomic.Int32
			resolver.stageURLs = func(context.Context, []ManagedFileURLSource, []ManagedInlineFileDescriptor, ManagedInlineLimits, string) (*ManagedInlineStaging, error) {
				downloads.Add(1)
				return nil, ErrInvalidFile
			}
			plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, []byte(body))
			require.Error(t, err)
			require.Nil(t, plan)
			require.Zero(t, downloads.Load())
			opened, _ := repo.readActivity()
			require.Empty(t, opened)
			require.Zero(t, ledger.lookupCalls)
			entries, err := os.ReadDir(files.cfg.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestManagedFileURLResolverRejectsSameLengthFinalURLMutationBeforeBPS(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}))
	defer source.Close()
	var remoteCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { remoteCalls.Add(1); w.WriteHeader(500) }))
	defer backend.Close()
	files := managedResolverFiles(t, &managedResolverFileRepo{files: map[string]*StoredFile{}})
	ledger := &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{}}
	resolver := NewManagedFileResolver(files, ledger, &bpsFilesTestTransport{client: backend.Client()}, &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}})
	var downloads atomic.Int32
	managedURLIntegrationDownloader(resolver, source, &downloads)
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"http://files.example/hello.txt?key=aaaa"}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	defer plan.Close()
	changed := []byte(strings.Replace(string(body), "key=aaaa", "key=bbbb", 1))
	require.Len(t, changed, len(body))
	result, err := resolver.Resolve(managedResolverAuthorizedContext(), plan, managedInlineTestAccount(backend.URL), changed)
	require.ErrorIs(t, err, ErrManagedFileUnsupportedInput)
	require.Nil(t, result)
	require.Zero(t, remoteCalls.Load())
	require.EqualValues(t, 1, downloads.Load())
	require.Nil(t, ledger.inline)
}

func TestManagedFileURLResolverCombinesPublicInlineAndURLBudgets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limits    ManagedInlineLimits
		rejected  bool
		downloads int32
	}{
		{name: "exact mixed budget", limits: ManagedInlineLimits{TotalBytes: 20, MessageBytes: 20, UniqueFiles: 3}, downloads: 1},
		{name: "public occurrence request bytes", limits: ManagedInlineLimits{TotalBytes: 19}, rejected: true, downloads: 1},
		{name: "public occurrence message bytes", limits: ManagedInlineLimits{MessageBytes: 19}, rejected: true, downloads: 1},
		{name: "three source kinds unique count", limits: ManagedInlineLimits{UniqueFiles: 2}, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "hello")
			}))
			defer source.Close()
			repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-public": managedResolverRecord("file-public")}}
			files := managedResolverFiles(t, repo)
			resolver := NewManagedFileResolver(files, &managedResolverLedger{}, nil, nil)
			var downloads atomic.Int32
			resolver.stageURLs = func(ctx context.Context, sources []ManagedFileURLSource, existing []ManagedInlineFileDescriptor, _ ManagedInlineLimits, parent string) (*ManagedInlineStaging, error) {
				return stageManagedFileURLs(ctx, sources, existing, tc.limits, parent, managedFileURLTestNetwork(source, nil, &downloads))
			}
			body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"http://files.example/hello.txt"},{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"inline.txt","file_data":"d29ybGQ="},{"type":"input_file","file_id":"file-public"}]}]}`)
			plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
			if tc.rejected {
				require.Error(t, err)
				require.Nil(t, plan)
			} else {
				require.NoError(t, err)
				require.NotNil(t, plan)
				require.NoError(t, plan.Close())
			}
			require.Equal(t, tc.downloads, downloads.Load())
			entries, err := os.ReadDir(files.cfg.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestManagedFileURLResolverCancellationCleansInlineAndPartialDownload(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strings.Repeat("a", 65536))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer source.Close()
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
	files := managedResolverFiles(t, repo)
	resolver := NewManagedFileResolver(files, &managedResolverLedger{}, nil, nil)
	var downloads atomic.Int32
	managedURLIntegrationDownloader(resolver, source, &downloads)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		plan, err := resolver.Prepare(ctx, FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"inline.txt","file_data":"d29ybGQ="},{"type":"input_file","file_url":"http://files.example/partial.txt"}]}]}`))
		if plan != nil {
			_ = plan.Close()
		}
		finished <- err
	}()
	require.Eventually(t, func() bool { entries, err := os.ReadDir(files.cfg.Root); return err == nil && len(entries) == 2 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled preparation did not finish")
	}
	entries, err := os.ReadDir(files.cfg.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.EqualValues(t, 1, downloads.Load())
}

type managedURLBackendObservation struct {
	mu        sync.Mutex
	manifests []managedfiles.AdmitManifest
	bytes     map[managedfiles.Position][]string
	ensures   int
}

func managedURLIntegrationBackend(t *testing.T, recoverMissing bool) (*httptest.Server, *managedURLBackendObservation) {
	t.Helper()
	observed := &managedURLBackendObservation{bytes: make(map[managedfiles.Position][]string)}
	var manifest managedfiles.AdmitManifest
	var results []managedfiles.EnsureResult
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed.mu.Lock()
		defer observed.mu.Unlock()
		fail := func(err error) {
			t.Error(err)
			http.Error(w, "invalid fixture request", http.StatusInternalServerError)
		}
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			description := bpsFilesTestDescription()
			description.Capabilities.MetadataRecovery = recoverMissing
			if err := json.NewEncoder(w).Encode(description); err != nil {
				fail(err)
			}
		case managedfiles.Prefix + "admit":
			mr, err := r.MultipartReader()
			if err != nil {
				fail(err)
				return
			}
			part, err := mr.NextPart()
			if err != nil {
				fail(err)
				return
			}
			raw, err := io.ReadAll(part)
			if err != nil {
				fail(err)
				return
			}
			manifest, err = managedfiles.DecodeManifest(raw)
			if err != nil {
				fail(err)
				return
			}
			observed.manifests = append(observed.manifests, manifest)
			for _, file := range manifest.Files {
				if file.Mode != "upload" {
					continue
				}
				part, err = mr.NextPart()
				if err != nil {
					fail(err)
					return
				}
				if part.FormName() != file.Part {
					t.Errorf("wrong multipart part %s for %s", part.FormName(), file.Part)
					w.WriteHeader(500)
					return
				}
				data, err := io.ReadAll(part)
				if err != nil {
					fail(err)
					return
				}
				for _, position := range file.Positions {
					observed.bytes[position] = append(observed.bytes[position], string(data))
				}
			}
			if _, err = mr.NextPart(); err != io.EOF {
				if err != nil {
					fail(err)
				} else {
					t.Error("unexpected multipart file")
					w.WriteHeader(500)
				}
				return
			}
			if recoverMissing && len(observed.manifests) == 1 {
				recoveryRoundMissing(t, w, manifest, "file-public")
				return
			}
			recoveryRoundAccepted(t, w, manifest)
		case managedfiles.Prefix + "ensure-uploaded":
			var request managedfiles.EnsureRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				fail(err)
				return
			}
			var file managedfiles.File
			uploads := 0
			for _, candidate := range manifest.Files {
				if candidate.Mode == "upload" {
					uploads++
				}
				if candidate.FileRef == request.FileRef {
					file = candidate
				}
			}
			if file.FileRef == "" {
				t.Error("unknown ensure file")
				w.WriteHeader(500)
				return
			}
			result := managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: manifest.Expected, OperationID: request.OperationID, FileRef: file.FileRef, RemoteID: "file-url-remote-" + file.FileRef, Bytes: file.Bytes, SHA256: file.SHA256, Filename: file.Filename, MIME: file.MIME, InputTokens: 2}
			results = append(results, result)
			observed.ensures++
			if len(results) == uploads {
				bound, err := managedfiles.Bind(manifest, results)
				if err != nil {
					fail(err)
					return
				}
				result.Ready, result.WireSHA256, result.BindingSHA256 = true, bound.WireSHA256, bound.BindingSHA256
			}
			if err := json.NewEncoder(w).Encode(result); err != nil {
				fail(err)
			}
		default:
			t.Errorf("unexpected private request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	return server, observed
}

func TestManagedFileURLResolverResolvesMixedSourcesAndReusesDownloadsDuringRecovery(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mixed, recovery bool
	}{
		{name: "pure URL"}, {name: "URL public inline URL public order", mixed: true}, {name: "recover missing public without refetch", mixed: true, recovery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Bps-Files-Key"} {
					if r.Header.Get(header) != "" {
						t.Errorf("URL forwarded credential header %s", header)
					}
				}
				body := "hello"
				if r.URL.Path == "/second.txt" {
					body = "other"
				}
				io.WriteString(w, body)
			}))
			defer source.Close()
			backend, observed := managedURLIntegrationBackend(t, tc.recovery)
			defer backend.Close()
			repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
			if tc.mixed {
				repo.files["file-public"] = managedResolverRecord("file-public")
			}
			ledger := &integratedRecoveryLedger{managedInlineResolverLedger: &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{existing: tc.recovery}}}
			files := managedResolverFiles(t, repo)
			resolver := NewManagedFileResolver(files, ledger, &bpsFilesTestTransport{client: backend.Client()}, &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}})
			var downloads atomic.Int32
			managedURLIntegrationDownloader(resolver, source, &downloads)
			parts := `{"type":"input_file","file_url":"http://files.example/first.txt?signature=private-value"}`
			if tc.mixed {
				parts += `,{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"inline.txt","file_data":"d29ybGQ="},{"type":"input_file","file_url":"http://files.example/second.txt"},{"type":"input_file","file_id":"file-public"}`
			}
			parts += `,{"type":"input_image","image_url":"http://images.example/untouched.png"},{"type":"input_text","text":"{\"type\":\"input_file\",\"file_url\":\"opaque text\"}"}`
			body := []byte(`{"model":"fixture-model","store":false,"input":[{"role":"user","content":[` + parts + `]}],"metadata":{"file_url":"opaque sidecar","large":9007199254740993}}`)
			ctx := managedResolverAuthorizedContext()
			plan, err := resolver.Prepare(ctx, FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
			require.NoError(t, err)
			defer plan.Close()
			result, err := resolver.Resolve(ctx, plan, managedInlineTestAccount(backend.URL), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NoError(t, result.Authorize(ctx))
			wire, err := managedfiles.Parse(result.Body)
			require.NoError(t, err)
			root := wire.(map[string]any)
			content := root["input"].([]any)[0].(map[string]any)["content"].([]any)
			wantDownloads, wantInline, wantAdmits := 1, 1, 1
			if tc.mixed {
				wantDownloads, wantInline = 2, 3
			}
			if tc.recovery {
				wantAdmits = 2
			}
			require.EqualValues(t, wantDownloads, downloads.Load(), "metadata recovery must reopen downloaded staging rather than fetch again")
			require.Equal(t, map[string]any{"type": "input_image", "image_url": "http://images.example/untouched.png"}, content[len(content)-2])
			require.Equal(t, map[string]any{"type": "input_text", "text": `{"type":"input_file","file_url":"opaque text"}`}, content[len(content)-1])
			require.Equal(t, "opaque sidecar", root["metadata"].(map[string]any)["file_url"])
			require.Contains(t, string(result.Body), "9007199254740993")
			require.NotContains(t, string(result.Body), "private-value")
			require.NotContains(t, content[0], "file_url")
			require.Contains(t, content[0].(map[string]any)["file_id"], "file-url-remote-")
			if tc.mixed {
				require.NotContains(t, content[2], "file_data")
				require.NotContains(t, content[3], "file_url")
			}
			observed.mu.Lock()
			manifests, received, ensures := observed.manifests, observed.bytes, observed.ensures
			observed.mu.Unlock()
			require.Len(t, manifests, wantAdmits)
			require.Equal(t, "hello", received[managedfiles.Position{Input: 0, Part: 0}][0])
			require.Len(t, received[managedfiles.Position{Input: 0, Part: 0}], wantAdmits)
			wantEnsures := wantInline
			if tc.mixed {
				wantEnsures++
				require.Equal(t, "world", received[managedfiles.Position{Input: 0, Part: 2}][0])
				require.Equal(t, "other", received[managedfiles.Position{Input: 0, Part: 3}][0])
				require.Len(t, received[managedfiles.Position{Input: 0, Part: 2}], wantAdmits)
				require.Len(t, received[managedfiles.Position{Input: 0, Part: 3}], wantAdmits)
				last := manifests[len(manifests)-1]
				require.Equal(t, []managedfiles.Position{{Input: 0, Part: 1}, {Input: 0, Part: 4}}, last.Files[0].Positions)
				require.Equal(t, content[1].(map[string]any)["file_id"], content[4].(map[string]any)["file_id"])
			}
			require.Equal(t, wantEnsures, ensures)
			require.Len(t, ledger.inlineCompletes, wantInline)
			if tc.recovery {
				require.Equal(t, 1, ledger.recoveries)
				require.Same(t, ledger.replacement, plan.reservation)
			}
			require.NoError(t, plan.Close())
			entries, err := os.ReadDir(files.cfg.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestManagedFileURLResolverPreparedCancellationClosesBothStages(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}))
	defer source.Close()
	files := managedResolverFiles(t, &managedResolverFileRepo{files: map[string]*StoredFile{}})
	resolver := NewManagedFileResolver(files, &managedResolverLedger{}, nil, nil)
	managedURLIntegrationDownloader(resolver, source, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"http://files.example/hello.txt"},{"type":"input_file","filename":"inline.txt","file_data":"d29ybGQ="}]}]}`)
	plan, err := resolver.Prepare(ctx, FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	defer plan.Close()
	urlReader, err := plan.staging.Open(0)
	require.NoError(t, err)
	inlineReader, err := plan.staging.Open(1)
	require.NoError(t, err)
	cancel()
	require.Eventually(t, func() bool { entries, err := os.ReadDir(files.cfg.Root); return err == nil && len(entries) == 0 }, 5*time.Second, 10*time.Millisecond)
	for _, reader := range []io.ReadCloser{urlReader, inlineReader} {
		_, err := reader.Read(make([]byte, 1))
		require.Error(t, err)
		require.NoError(t, reader.Close())
	}
	_, err = plan.staging.Open(0)
	require.Error(t, err)
	require.NoError(t, plan.Close())
}

func TestManagedFileURLResolverPreparesDownloadedBytesWithoutPermanentFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}))
	defer server.Close()
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
	files := managedResolverFiles(t, repo)
	ledger := &managedResolverLedger{}
	resolver := NewManagedFileResolver(files, ledger, nil, nil)
	var downloads atomic.Int32
	managedURLIntegrationDownloader(resolver, server, &downloads)
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"http://files.example/hello.txt?signature=private-value"}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	require.NotNil(t, plan)
	defer plan.Close()
	staged := plan.staging.Files()
	require.Len(t, staged, 1)
	require.EqualValues(t, 5, staged[0].Bytes)
	require.Equal(t, "text/plain", staged[0].MIME)
	require.Equal(t, "hello.txt", staged[0].Filename)
	r, err := plan.staging.Open(0)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, "hello", string(data))
	require.EqualValues(t, 1, downloads.Load())
	require.Zero(t, ledger.lookupCalls)
	require.Empty(t, repo.files)
	require.NoError(t, plan.Close())
	entries, err := os.ReadDir(files.cfg.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
}
