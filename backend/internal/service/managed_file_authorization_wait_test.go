//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

// Returning from the operation models a database wait whose authority was
// revoked before the row lock or transaction finally completed.
type managedAuthorizationWaitLedger struct {
	*managedInlineResolverLedger
	revoked, revokeAtStart, revokeAtAuthorization          atomic.Bool
	starts, completes, uncertain, authorizations, releases atomic.Int32
}

func (l *managedAuthorizationWaitLedger) waitStart() error {
	l.starts.Add(1)
	if l.revokeAtStart.Load() {
		l.revoked.Store(true)
	}
	return nil
}
func (l *managedAuthorizationWaitLedger) StartOperation(context.Context, *ManagedFileReservation, string) error {
	return l.waitStart()
}
func (l *managedAuthorizationWaitLedger) StartInline(context.Context, *ManagedInlineReservation, string) error {
	return l.waitStart()
}
func (l *managedAuthorizationWaitLedger) CompleteOperation(context.Context, *ManagedFileReservation, string, managedfiles.EnsureResult) (bool, error) {
	l.completes.Add(1)
	return true, nil
}
func (l *managedAuthorizationWaitLedger) CompleteInline(context.Context, *ManagedInlineReservation, string, managedfiles.EnsureResult) (bool, error) {
	l.completes.Add(1)
	return true, nil
}
func (l *managedAuthorizationWaitLedger) MarkUncertain(context.Context, *ManagedFileReservation, string, string) error {
	l.uncertain.Add(1)
	return nil
}
func (l *managedAuthorizationWaitLedger) MarkInlineUncertain(context.Context, *ManagedInlineReservation, string, string) error {
	l.uncertain.Add(1)
	return nil
}
func (l *managedAuthorizationWaitLedger) waitAuthorize() error {
	l.authorizations.Add(1)
	if l.revokeAtAuthorization.Load() {
		l.revoked.Store(true)
	}
	return nil
}
func (l *managedAuthorizationWaitLedger) AuthorizeGeneration(context.Context, *ManagedFileReservation) error {
	return l.waitAuthorize()
}
func (l *managedAuthorizationWaitLedger) AuthorizeInlineGeneration(context.Context, *ManagedInlineReservation) error {
	return l.waitAuthorize()
}
func (l *managedAuthorizationWaitLedger) ReleaseReservation(context.Context, *ManagedFileReservation) error {
	l.releases.Add(1)
	return nil
}
func (l *managedAuthorizationWaitLedger) ReleaseInline(context.Context, *ManagedInlineReservation) error {
	l.releases.Add(1)
	return nil
}

type managedAuthorizationWaitFixture struct {
	resolver             *ManagedFileResolver
	plan                 *ManagedFilePlan
	ledger               *managedAuthorizationWaitLedger
	ctx                  context.Context
	account              *Account
	body                 []byte
	ensures, generations atomic.Int32
}

func newManagedAuthorizationWaitFixture(t *testing.T, inline bool) *managedAuthorizationWaitFixture {
	t.Helper()
	f := &managedAuthorizationWaitFixture{ledger: &managedAuthorizationWaitLedger{managedInlineResolverLedger: &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{}}}}
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
	content := `{"type":"input_file","file_id":"file-wait"}`
	if inline {
		content = `{"type":"input_file","filename":"wait.txt","file_data":"aGVsbG8="}`
	} else {
		repo.files["file-wait"] = managedResolverRecord("file-wait")
	}
	f.body = []byte(`{"model":"gpt-5.5","store":false,"input":[{"role":"user","content":[` + content + `]}]}`)
	var admitted atomic.Pointer[managedfiles.AdmitManifest]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			require.NoError(t, json.NewEncoder(w).Encode(bpsFilesTestDescription()))
		case managedfiles.Prefix + "admit":
			mr, err := r.MultipartReader()
			require.NoError(t, err)
			part, err := mr.NextPart()
			require.NoError(t, err)
			raw, err := io.ReadAll(part)
			require.NoError(t, err)
			manifest, err := managedfiles.DecodeManifest(raw)
			require.NoError(t, err)
			require.Len(t, manifest.Files, 1)
			part, err = mr.NextPart()
			require.NoError(t, err)
			content, err := io.ReadAll(part)
			require.NoError(t, err)
			require.Equal(t, "hello", string(content))
			_, err = mr.NextPart()
			require.ErrorIs(t, err, io.EOF)
			admitted.Store(&manifest)
			hash, err := managedfiles.TemplateHash(manifest.RequestTemplate)
			require.NoError(t, err)
			require.NoError(t, json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "wait-admission", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: []managedfiles.AdmittedFile{{FileRef: manifest.Files[0].FileRef, State: "staged"}}}))
		case managedfiles.Prefix + "ensure-uploaded":
			f.ensures.Add(1)
			var req managedfiles.EnsureRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			manifest := admitted.Load()
			require.NotNil(t, manifest)
			file := manifest.Files[0]
			result := managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: bpsFilesTestIdentity(), OperationID: req.OperationID, FileRef: file.FileRef, RemoteID: "file-wait-remote", Bytes: file.Bytes, SHA256: file.SHA256, Filename: file.Filename, MIME: file.MIME, Ready: true}
			bound, err := managedfiles.Bind(*manifest, []managedfiles.EnsureResult{result})
			require.NoError(t, err)
			result.WireSHA256 = bound.WireSHA256
			result.BindingSHA256 = bound.BindingSHA256
			require.NoError(t, json.NewEncoder(w).Encode(result))
		default:
			f.generations.Add(1)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	f.resolver = NewManagedFileResolver(managedResolverFiles(t, repo), f.ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	f.ctx = withManagedFilePreparationAuthorization(context.Background(), func(context.Context) error {
		if f.ledger.revoked.Load() {
			return ErrBPSRequestAuthorizationRevoked
		}
		return nil
	})
	var err error
	f.plan, err = f.resolver.Prepare(f.ctx, FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, f.body)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.plan.Close()) })
	f.account = managedInlineTestAccount(server.URL)
	return f
}

func TestManagedFileResolverRechecksAuthorityAfterStartDatabaseWait(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(map[bool]string{false: "permanent operation", true: "inline operation"}[inline], func(t *testing.T) {
			f := newManagedAuthorizationWaitFixture(t, inline)
			f.ledger.revokeAtStart.Store(true)
			resolved, err := f.resolver.Resolve(f.ctx, f.plan, f.account, f.body)
			require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
			require.Nil(t, resolved)
			require.EqualValues(t, 1, f.ledger.starts.Load())
			require.Zero(t, f.ensures.Load(), "revocation during durable Start must prevent Ensure")
			require.Zero(t, f.ledger.completes.Load())
			require.Zero(t, f.generations.Load())
			require.NoError(t, f.plan.Close())
			require.EqualValues(t, 1, f.ledger.releases.Load(), "started operation must retain the permit for safe release")
		})
	}
}

func TestManagedFileResolvedRechecksAuthorityAfterAuthorizationDatabaseWait(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(map[bool]string{false: "permanent authorization", true: "inline authorization"}[inline], func(t *testing.T) {
			f := newManagedAuthorizationWaitFixture(t, inline)
			resolved, err := f.resolver.Resolve(f.ctx, f.plan, f.account, f.body)
			require.NoError(t, err)
			require.NotNil(t, resolved)
			before := f.ledger.authorizations.Load()
			f.ledger.revokeAtAuthorization.Store(true)
			err = resolved.Authorize(f.ctx)
			require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked, "an earlier authority check cannot survive a later ledger wait")
			require.EqualValues(t, before+1, f.ledger.authorizations.Load())
			require.EqualValues(t, 1, f.ensures.Load())
			require.Zero(t, f.generations.Load())
		})
	}
}
