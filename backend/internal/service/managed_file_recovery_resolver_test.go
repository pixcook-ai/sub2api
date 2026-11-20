package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

type recoveryRoundLedger struct {
	ManagedFileRepository
	reserveCalls int
	installed    *ManagedFileReservation
	released     *ManagedFileReservation
}

func (l *recoveryRoundLedger) ReserveRecovery(_ context.Context, in ManagedFileRecoveryInput) (*ManagedFileReservation, error) {
	l.reserveCalls++
	copy := *in.Reservation
	copy.Files = append([]ManagedFileBinding(nil), copy.Files...)
	for i := range copy.Files {
		for _, ref := range in.FileRefs {
			if copy.Files[i].File.PublicID == ref {
				copy.Files[i].Version++
				copy.Files[i].Operation = ManagedFileOperation{ID: "recovery-operation", State: "reserved"}
			}
		}
	}
	l.installed = &copy
	return &copy, nil
}

func (l *recoveryRoundLedger) ReleaseReservation(_ context.Context, permit *ManagedFileReservation) error {
	l.released = permit
	return nil
}

func recoveryRoundFixture(t *testing.T, handler http.Handler) (*ManagedFilePlan, *BPSFilesBackend, managedfiles.AdmitManifest, *ManagedFileReservation, *recoveryRoundLedger) {
	t.Helper()
	input := managedRecoveryInput()
	permit := input.Reservation
	permit.Selection.Identity = bpsFilesTestIdentity()
	permit.Selection.AccountID = 7
	f := managedResolverRecord("file-recovery")
	f.ID = 19
	permit.Files[0].File = f
	permit.Files[0].Selection = permit.Selection
	manifest := managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: "request-recovery", Expected: permit.Selection.Identity, OwnerRef: "1", GroupRef: "2", RequestTemplate: json.RawMessage(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-recovery"}]}]}`), Files: []managedfiles.File{{FileRef: f.PublicID, Mode: "existing", Filename: f.Filename, MIME: f.MIME, Bytes: f.Bytes, SHA256: f.SHA256, RemoteID: permit.Files[0].Operation.RemoteID, OperationID: permit.Files[0].Operation.ID, Positions: []managedfiles.Position{{Input: 0, Part: 0}}}}}
	ledger := &recoveryRoundLedger{}
	plan := &ManagedFilePlan{owner: permit.Owner, files: []*StoredFile{f}, readers: map[string]io.ReadCloser{f.PublicID: io.NopCloser(strings.NewReader("hello"))}, reservation: permit, resolver: &ManagedFileResolver{ledger: ledger}}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return plan, bpsFilesTestBackend(t, server.URL, &bpsFilesTestTransport{client: server.Client()}), manifest, permit, ledger
}

func recoveryRoundReadManifest(t *testing.T, r *http.Request) managedfiles.AdmitManifest {
	t.Helper()
	mr, err := r.MultipartReader()
	require.NoError(t, err)
	part, err := mr.NextPart()
	require.NoError(t, err)
	raw, err := io.ReadAll(part)
	require.NoError(t, err)
	m, err := managedfiles.DecodeManifest(raw)
	require.NoError(t, err)
	for {
		p, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		require.NoError(t, e)
		_, e = io.Copy(io.Discard, p)
		require.NoError(t, e)
	}
	return m
}

func recoveryRoundMissing(t *testing.T, w http.ResponseWriter, m managedfiles.AdmitManifest, refs ...string) {
	t.Helper()
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	canonical, err := managedfiles.Canonical(raw)
	require.NoError(t, err)
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	if len(refs) == 0 {
		refs = []string{"file-recovery"}
	}
	w.WriteHeader(http.StatusConflict)
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "managed_metadata_missing", "param": nil, "message": "owner-scoped native metadata is unavailable", "recovery": map[string]any{"contract": "bps-metadata-recovery/1", "identity": m.Expected, "request_id": m.RequestID, "manifest_sha256": hash, "missing_refs": refs}}}))
}

func TestManagedFileRecoveryRoundInstallsReplacementPermitBeforeSecondAdmitFailure(t *testing.T) {
	calls := 0
	var plan *ManagedFilePlan
	var ledger *recoveryRoundLedger
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		m := recoveryRoundReadManifest(t, r)
		if calls == 1 {
			recoveryRoundMissing(t, w, m)
			return
		}
		require.Same(t, ledger.installed, plan.reservation, "Close must already own the new fence before another HTTP call")
		require.Equal(t, "upload", m.Files[0].Mode)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var backend *BPSFilesBackend
	var manifest managedfiles.AdmitManifest
	var permit *ManagedFileReservation
	plan, backend, manifest, permit, ledger = recoveryRoundFixture(t, handler)
	limits := bpsFilesTestDescription().Limits
	opened := 0
	result, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: manifest, Reservation: permit, Limits: limits, Authorize: func(context.Context) error { return nil }, Open: func(context.Context, string) (managedfiles.File, io.ReadCloser, error) {
		opened++
		return manifest.Files[0], io.NopCloser(strings.NewReader("hello")), nil
	}})
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, opened)
	require.Equal(t, 1, ledger.reserveCalls)
	require.NoError(t, plan.Close())
	require.Same(t, ledger.installed, ledger.released)
	require.True(t, time.Now().Before(permit.Files[0].LeaseUntil))
}

type recoveryRoundReader struct {
	io.Reader
	closed int
}

func (r *recoveryRoundReader) Close() error { r.closed++; return nil }

func recoveryRoundAccepted(t *testing.T, w http.ResponseWriter, m managedfiles.AdmitManifest) {
	t.Helper()
	hash, err := managedfiles.TemplateHash(m.RequestTemplate)
	require.NoError(t, err)
	result := managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: m.Expected, AdmissionID: "replacement-admission", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash}
	allExisting := true
	for _, f := range m.Files {
		state := "known"
		if f.Mode == "upload" {
			state = "staged"
			allExisting = false
		}
		result.Files = append(result.Files, managedfiles.AdmittedFile{FileRef: f.FileRef, State: state})
	}
	if allExisting {
		bound, err := managedfiles.Bind(m, nil)
		require.NoError(t, err)
		result.Ready, result.WireSHA256, result.BindingSHA256 = true, bound.WireSHA256, bound.BindingSHA256
	}
	require.NoError(t, json.NewEncoder(w).Encode(result))
}

func recoveryRoundMixed(t *testing.T, plan *ManagedFilePlan, m *managedfiles.AdmitManifest, permit *ManagedFileReservation) {
	t.Helper()
	public := managedResolverRecord("file-original-upload")
	public.ID = 20
	b := permit.Files[0]
	b.File = public
	b.Operation = ManagedFileOperation{ID: "original-upload-operation", State: "reserved"}
	permit.Files = append(permit.Files, b)
	plan.files = append(plan.files, public)
	m.RequestTemplate = json.RawMessage(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-recovery"},{"type":"input_file","file_id":"file-original-upload"},{"type":"input_file","file_id":"file-inline-request"}]}]}`)
	for i, ref := range []string{public.PublicID, "file-inline-request"} {
		f := m.Files[0]
		f.FileRef, f.Mode, f.Part, f.RemoteID, f.OperationID = ref, "upload", fmt.Sprintf("recovery_%d", i), "", ""
		f.Positions = []managedfiles.Position{{Input: 0, Part: i + 1}}
		m.Files = append(m.Files, f)
		plan.readers[ref] = &recoveryRoundReader{Reader: strings.NewReader("hello")}
	}
}

func TestManagedFileRecoveryRoundReopensEveryConsumedPublicAndInlineUpload(t *testing.T) {
	calls := 0
	seen := make([]map[string]string, 0, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		mr, err := r.MultipartReader()
		require.NoError(t, err)
		first, err := mr.NextPart()
		require.NoError(t, err)
		raw, err := io.ReadAll(first)
		require.NoError(t, err)
		m, err := managedfiles.DecodeManifest(raw)
		require.NoError(t, err)
		payloads := map[string]string{}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			data, err := io.ReadAll(p)
			require.NoError(t, err)
			payloads[p.FormName()] = string(data)
		}
		seen = append(seen, payloads)
		if calls == 1 {
			recoveryRoundMissing(t, w, m)
			return
		}
		require.Len(t, payloads, 3)
		for _, f := range m.Files {
			require.Equal(t, "hello", payloads[f.Part])
		}
		recoveryRoundAccepted(t, w, m)
	})
	plan, backend, m, permit, ledger := recoveryRoundFixture(t, handler)
	recoveryRoundMixed(t, plan, &m, permit)
	old := make([]*recoveryRoundReader, 0, 2)
	for _, ref := range []string{"file-original-upload", "file-inline-request"} {
		reader, ok := plan.readers[ref].(*recoveryRoundReader)
		require.True(t, ok)
		old = append(old, reader)
	}
	opened := map[string]int{}
	reopened := map[string]*recoveryRoundReader{}
	authorized := 0
	result, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits, Authorize: func(context.Context) error { authorized++; return nil }, Open: func(_ context.Context, ref string) (managedfiles.File, io.ReadCloser, error) {
		opened[ref]++
		reader := &recoveryRoundReader{Reader: strings.NewReader("hello")}
		reopened[ref] = reader
		for _, f := range m.Files {
			if f.FileRef == ref {
				return f, reader, nil
			}
		}
		return managedfiles.File{}, reader, ErrFileNotFound
	}})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Len(t, seen[0], 2, "first request must actually consume both original upload sources")
	require.Len(t, seen[1], 3)
	require.Equal(t, map[string]int{"file-recovery": 1, "file-original-upload": 1, "file-inline-request": 1}, opened)
	require.Equal(t, 2, authorized)
	require.Same(t, ledger.installed, result.Reservation)
	require.Same(t, ledger.installed, plan.reservation)
	require.Equal(t, "replacement-admission", result.Admission.AdmissionID)
	require.Equal(t, m.Files[1:], result.Manifest.Files[1:])
	require.Equal(t, "recovery_2", result.Manifest.Files[0].Part, "new upload parts must not collide with old upload parts")
	require.Equal(t, "existing", m.Files[0].Mode, "the caller's old manifest is immutable")
	for _, reader := range old {
		require.Equal(t, 1, reader.closed)
	}
	for _, reader := range reopened {
		require.Zero(t, reader.closed)
	}
	require.NoError(t, plan.Close())
	for _, reader := range reopened {
		require.Equal(t, 1, reader.closed)
	}
}

func TestManagedFileRecoveryRoundRecomputesUploadBudgetBeforeReopening(t *testing.T) {
	calls := 0
	plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		recoveryRoundMissing(t, w, recoveryRoundReadManifest(t, r))
	}))
	limits := bpsFilesTestDescription().Limits
	limits.AdmissionBytes = 4
	opened := 0
	_, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: limits, Authorize: func(context.Context) error { return nil }, Open: func(context.Context, string) (managedfiles.File, io.ReadCloser, error) {
		opened++
		return managedfiles.File{}, nil, nil
	}})
	var problem *BPSFilesBackendError
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "admission_too_large", problem.Code)
	require.Equal(t, 1, calls)
	require.Zero(t, opened)
	require.Equal(t, 1, ledger.reserveCalls)
	require.Same(t, ledger.installed, plan.reservation)
	require.NoError(t, plan.Close())
	require.Same(t, ledger.installed, ledger.released)
}

func TestManagedFileRecoveryRoundStopsAfterOneRecovery(t *testing.T) {
	calls := 0
	plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		m := recoveryRoundReadManifest(t, r)
		if calls == 1 {
			recoveryRoundMissing(t, w, m)
			return
		}
		// The old missing ref is now an upload: even a syntactically exact
		// repeated envelope cannot authorize another durable recovery.
		recoveryRoundMissing(t, w, m)
	}))
	_, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits, Authorize: func(context.Context) error { return nil }, Open: func(context.Context, string) (managedfiles.File, io.ReadCloser, error) {
		return m.Files[0], io.NopCloser(strings.NewReader("hello")), nil
	}})
	require.Error(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, ledger.reserveCalls)
	require.NoError(t, plan.Close())
}

func TestManagedFileRecoveryRoundStopsOnAnotherPreciselyMissingExistingFile(t *testing.T) {
	calls := 0
	plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		m := recoveryRoundReadManifest(t, r)
		if calls == 1 {
			recoveryRoundMissing(t, w, m)
			return
		}
		recoveryRoundMissing(t, w, m, "file-other-existing")
	}))
	other := managedResolverRecord("file-other-existing")
	other.ID = 21
	b := permit.Files[0]
	b.File = other
	b.Operation = ManagedFileOperation{ID: "operation-other", State: "succeeded", RemoteID: "file-native-other"}
	permit.Files = append(permit.Files, b)
	plan.files = append(plan.files, other)
	f := m.Files[0]
	f.FileRef = other.PublicID
	f.OperationID = b.Operation.ID
	f.RemoteID = b.Operation.RemoteID
	f.Positions = []managedfiles.Position{{Input: 0, Part: 1}}
	m.Files = append(m.Files, f)
	m.RequestTemplate = json.RawMessage(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-recovery"},{"type":"input_file","file_id":"file-other-existing"}]}]}`)
	_, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits, Authorize: func(context.Context) error { return nil }, Open: func(context.Context, string) (managedfiles.File, io.ReadCloser, error) {
		return m.Files[0], io.NopCloser(strings.NewReader("hello")), nil
	}})
	require.ErrorIs(t, err, ErrManagedFileNotReady)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, ledger.reserveCalls)
	require.Same(t, ledger.installed, plan.reservation)
	require.NoError(t, plan.Close())
}

func TestManagedFileRecoveryRoundFreshAuthorizationFailureStopsBeforeReplacementAdmit(t *testing.T) {
	for _, denyAt := range []int{1, 2} {
		t.Run(fmt.Sprint(denyAt), func(t *testing.T) {
			calls := 0
			denied := errors.New("current account no longer eligible")
			plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				recoveryRoundMissing(t, w, recoveryRoundReadManifest(t, r))
			}))
			authorized, opened := 0, 0
			reader := &recoveryRoundReader{Reader: strings.NewReader("hello")}
			_, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits, Authorize: func(context.Context) error {
				authorized++
				if authorized == denyAt {
					return denied
				}
				return nil
			}, Open: func(context.Context, string) (managedfiles.File, io.ReadCloser, error) {
				opened++
				return m.Files[0], reader, nil
			}})
			require.ErrorIs(t, err, denied)
			require.Equal(t, 1, calls)
			require.Equal(t, denyAt-1, ledger.reserveCalls)
			require.Equal(t, denyAt-1, opened)
			if denyAt == 2 {
				require.Equal(t, 1, reader.closed)
				require.Same(t, ledger.installed, plan.reservation)
			} else {
				require.Same(t, permit, plan.reservation)
			}
			require.NoError(t, plan.Close())
			require.Same(t, plan.reservation, ledger.released)
		})
	}
}

func TestManagedFileRecoveryRoundClosesPartiallyReopenedSources(t *testing.T) {
	for _, failure := range []string{"blob missing", "descriptor changed", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				recoveryRoundMissing(t, w, recoveryRoundReadManifest(t, r))
			}))
			recoveryRoundMixed(t, plan, &m, permit)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opened := make([]*recoveryRoundReader, 0, 2)
			_, err := new(ManagedFileRecoveryRound).Admit(ctx, ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits, Authorize: func(context.Context) error { return nil }, Open: func(_ context.Context, ref string) (managedfiles.File, io.ReadCloser, error) {
				reader := &recoveryRoundReader{Reader: strings.NewReader("hello")}
				opened = append(opened, reader)
				for _, f := range m.Files {
					if f.FileRef == ref {
						if len(opened) == 2 {
							switch failure {
							case "blob missing":
								return f, reader, ErrFileNotFound
							case "descriptor changed":
								f.SHA256 = strings.Repeat("0", 64)
							case "cancelled":
								cancel()
							}
						}
						return f, reader, nil
					}
				}
				return managedfiles.File{}, reader, ErrFileNotFound
			}})
			require.Error(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, ledger.reserveCalls)
			require.Len(t, opened, 2)
			for _, reader := range opened {
				require.Equal(t, 1, reader.closed)
			}
			require.Same(t, ledger.installed, plan.reservation)
			require.NoError(t, plan.Close())
			require.Same(t, ledger.installed, ledger.released)
		})
	}
}

func TestManagedFileRecoveryRoundInitialSuccessDoesNotRecover(t *testing.T) {
	calls := 0
	plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		recoveryRoundAccepted(t, w, recoveryRoundReadManifest(t, r))
	}))
	round := new(ManagedFileRecoveryRound)
	result, err := round.Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Zero(t, ledger.reserveCalls)
	require.False(t, round.used.Load())
	require.Same(t, permit, result.Reservation)
	require.Equal(t, m, result.Manifest)
	require.True(t, result.Admission.Ready)
	require.NoError(t, plan.Close())
}

func TestManagedFileRecoveryRoundOrdinaryErrorsNeverMutateLedger(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"conflict"}}`, `{"error":{"code":"managed_metadata_missing"}}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = recoveryRoundReadManifest(t, r)
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, body)
			}))
			authorized, opened := 0, 0
			round := new(ManagedFileRecoveryRound)
			_, err := round.Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: bpsFilesTestDescription().Limits, Authorize: func(context.Context) error { authorized++; return nil }, Open: func(context.Context, string) (managedfiles.File, io.ReadCloser, error) {
				opened++
				return managedfiles.File{}, nil, nil
			}})
			require.Error(t, err)
			require.Equal(t, 1, calls)
			require.Zero(t, ledger.reserveCalls)
			require.Zero(t, authorized)
			require.Zero(t, opened)
			require.False(t, round.used.Load())
			require.Same(t, permit, plan.reservation)
			require.NoError(t, plan.Close())
		})
	}
}

func TestManagedFileRecoveryRoundRejectsInvalidFrozenFactsBeforeHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ManagedFilePlan, *BPSFilesBackend, *managedfiles.AdmitManifest, *ManagedFileReservation, *managedfiles.Limits)
	}{
		{"closed plan", func(p *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, _ *ManagedFileReservation, _ *managedfiles.Limits) {
			p.closed = true
		}},
		{"wrong owner", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, r *ManagedFileReservation, _ *managedfiles.Limits) {
			r.Owner.UserID++
		}},
		{"wrong account", func(_ *ManagedFilePlan, b *BPSFilesBackend, _ *managedfiles.AdmitManifest, _ *ManagedFileReservation, _ *managedfiles.Limits) {
			b.config.AccountID++
		}},
		{"wrong identity", func(_ *ManagedFilePlan, _ *BPSFilesBackend, m *managedfiles.AdmitManifest, _ *ManagedFileReservation, _ *managedfiles.Limits) {
			m.Expected.CredentialGeneration = "rotated"
		}},
		{"wrong remote", func(_ *ManagedFilePlan, _ *BPSFilesBackend, m *managedfiles.AdmitManifest, _ *ManagedFileReservation, _ *managedfiles.Limits) {
			m.Files[0].RemoteID = "file-other-native"
		}},
		{"wrong operation", func(_ *ManagedFilePlan, _ *BPSFilesBackend, m *managedfiles.AdmitManifest, _ *ManagedFileReservation, _ *managedfiles.Limits) {
			m.Files[0].OperationID = "other-op"
		}},
		{"retired file", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, r *ManagedFileReservation, _ *managedfiles.Limits) {
			now := time.Now()
			r.Files[0].File.RetiredAt = &now
		}},
		{"expired lease", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, r *ManagedFileReservation, _ *managedfiles.Limits) {
			r.Files[0].LeaseUntil = time.Now().Add(-time.Second)
		}},
		{"missing version", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, r *ManagedFileReservation, _ *managedfiles.Limits) {
			r.Files[0].Version = 0
		}},
		{"wrong hash", func(_ *ManagedFilePlan, _ *BPSFilesBackend, m *managedfiles.AdmitManifest, _ *ManagedFileReservation, _ *managedfiles.Limits) {
			m.Files[0].SHA256 = strings.Repeat("c", 64)
		}},
		{"invalid finite limits", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, _ *ManagedFileReservation, l *managedfiles.Limits) {
			l.AdmissionBytes = 0
		}},
		{"per file budget", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, _ *ManagedFileReservation, l *managedfiles.Limits) {
			l.FileBytes = 4
		}},
		{"per message budget", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, _ *ManagedFileReservation, l *managedfiles.Limits) {
			l.MessageBytes = 4
		}},
		{"manifest budget", func(_ *ManagedFilePlan, _ *BPSFilesBackend, _ *managedfiles.AdmitManifest, _ *ManagedFileReservation, l *managedfiles.Limits) {
			l.ManifestBytes = 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			plan, backend, m, permit, ledger := recoveryRoundFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			limits := bpsFilesTestDescription().Limits
			tc.change(plan, backend, &m, permit, &limits)
			_, err := new(ManagedFileRecoveryRound).Admit(context.Background(), ManagedFileRecoveryAdmissionInput{Plan: plan, Backend: backend, Manifest: m, Reservation: permit, Limits: limits})
			require.Error(t, err)
			require.Zero(t, calls)
			require.Zero(t, ledger.reserveCalls)
			require.NoError(t, plan.Close())
		})
	}
}
