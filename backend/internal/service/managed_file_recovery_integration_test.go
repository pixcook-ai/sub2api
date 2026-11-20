//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

type integratedRecoveryLedger struct {
	*managedInlineResolverLedger
	recoveries            int
	replacement, released *ManagedFileReservation
}

func (l *integratedRecoveryLedger) Reserve(ctx context.Context, in ManagedFileReserveInput) (*ManagedFileReservation, error) {
	p, err := l.managedResolverLedger.Reserve(ctx, in)
	if err != nil {
		return nil, err
	}
	for i := range p.Files {
		p.Files[i].LeaseToken = in.LeaseToken
		p.Files[i].LeaseUntil = time.Now().Add(in.Lease)
	}
	return p, nil
}

func (l *integratedRecoveryLedger) ReserveRecovery(_ context.Context, in ManagedFileRecoveryInput) (*ManagedFileReservation, error) {
	l.recoveries++
	p := *in.Reservation
	p.Files = append([]ManagedFileBinding(nil), p.Files...)
	for i := range p.Files {
		for _, ref := range in.FileRefs {
			if p.Files[i].File.PublicID == ref {
				p.Files[i].Version++
				p.Files[i].Operation = ManagedFileOperation{ID: "operation-recovered-public", State: "reserved"}
			}
		}
	}
	l.replacement = &p
	return &p, nil
}

func (l *integratedRecoveryLedger) ReleaseReservation(_ context.Context, p *ManagedFileReservation) error {
	l.released = p
	return nil
}

func TestManagedFileResolverRecoversMissingPublicWithFreshInlineBytes(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-recovery": managedResolverRecord("file-recovery")}}
	ledger := &integratedRecoveryLedger{managedInlineResolverLedger: &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{existing: true}}}
	var manifest managedfiles.AdmitManifest
	var results []managedfiles.EnsureResult
	admits, ensures := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			d := bpsFilesTestDescription()
			d.Capabilities.MetadataRecovery = true
			require.NoError(t, json.NewEncoder(w).Encode(d))
		case managedfiles.Prefix + "admit":
			admits++
			mr, err := r.MultipartReader()
			require.NoError(t, err)
			part, err := mr.NextPart()
			require.NoError(t, err)
			raw, err := io.ReadAll(part)
			require.NoError(t, err)
			manifest, err = managedfiles.DecodeManifest(raw)
			require.NoError(t, err)
			for _, f := range manifest.Files {
				if f.Mode != "upload" {
					continue
				}
				part, err = mr.NextPart()
				require.NoError(t, err)
				require.Equal(t, f.Part, part.FormName())
				data, err := io.ReadAll(part)
				require.NoError(t, err)
				want := "world"
				if f.FileRef == "file-recovery" {
					want = "hello"
				}
				require.Equal(t, want, string(data), "recovery must reopen the original inline upload too")
			}
			_, err = mr.NextPart()
			require.ErrorIs(t, err, io.EOF)
			if admits == 1 {
				require.Equal(t, "existing", manifest.Files[0].Mode)
				recoveryRoundMissing(t, w, manifest, "file-recovery")
				return
			}
			require.Equal(t, 2, admits)
			require.Equal(t, "upload", manifest.Files[0].Mode)
			require.NotNil(t, ledger.replacement)
			recoveryRoundAccepted(t, w, manifest)
		case managedfiles.Prefix + "ensure-uploaded":
			ensures++
			var req managedfiles.EnsureRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			var f managedfiles.File
			for _, candidate := range manifest.Files {
				if candidate.FileRef == req.FileRef {
					f = candidate
				}
			}
			require.NotEmpty(t, f.FileRef)
			if f.FileRef == "file-recovery" {
				require.Equal(t, "operation-recovered-public", req.OperationID)
				require.Equal(t, []string{"file-recovery"}, ledger.started)
			} else {
				require.Contains(t, ledger.inlineStarts, f.FileRef)
			}
			result := managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: manifest.Expected, OperationID: req.OperationID, FileRef: f.FileRef, RemoteID: "file-native-" + req.FileRef, Bytes: f.Bytes, SHA256: f.SHA256, Filename: f.Filename, MIME: f.MIME, InputTokens: 2}
			results = append(results, result)
			if len(results) == len(manifest.Files) {
				bound, err := managedfiles.Bind(manifest, results)
				require.NoError(t, err)
				result.Ready = true
				result.WireSHA256 = bound.WireSHA256
				result.BindingSHA256 = bound.BindingSHA256
			}
			require.NoError(t, json.NewEncoder(w).Encode(result))
		default:
			t.Errorf("unexpected private request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"model":"fixture-model","store":false,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-recovery"},{"type":"input_file","filename":"inline.txt","file_data":"d29ybGQ="}]}]}`)
	ctx := managedResolverAuthorizedContext()
	plan, err := resolver.Prepare(ctx, FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	defer plan.Close()
	resolved, err := resolver.Resolve(ctx, plan, managedInlineTestAccount(server.URL), body)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.Equal(t, 2, admits)
	require.Equal(t, 2, ensures)
	require.Equal(t, 1, ledger.recoveries)
	require.Same(t, ledger.replacement, plan.reservation)
	require.NoError(t, resolved.Authorize(ctx))
	require.Contains(t, string(resolved.Body), "file-native-file-recovery")
	require.False(t, strings.Contains(string(resolved.Body), "file_data"))
	require.NoError(t, plan.Close())
	require.Same(t, ledger.replacement, ledger.released)
	require.Equal(t, 1, ledger.inlineReleases)
}

func TestManagedFileResolverChecksLiveAuthorityBeforeRemoteSideEffects(t *testing.T) {
	for _, phase := range []string{"missing authority", "revoked before describe", "revoked after describe", "revoked before admission"} {
		t.Run(phase, func(t *testing.T) {
			repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
			ledger := &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{}}
			calls, checks := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == managedfiles.Prefix+"describe" {
					require.NoError(t, json.NewEncoder(w).Encode(bpsFilesTestDescription()))
					return
				}
				t.Errorf("revoked authority reached %s", r.URL.Path)
				w.WriteHeader(500)
			}))
			defer server.Close()
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
			resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
			body := []byte(`{"store":false,"input":[{"role":"user","content":[{"type":"input_file","filename":"inline.txt","file_data":"aGVsbG8="}]}]}`)
			plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
			require.NoError(t, err)
			defer plan.Close()
			ctx := context.Background()
			if phase != "missing authority" {
				ctx = withManagedFilePreparationAuthorization(ctx, func(context.Context) error {
					checks++
					threshold := 1
					if phase == "revoked after describe" {
						threshold = 2
					}
					if phase == "revoked before admission" {
						threshold = 3
					}
					if checks >= threshold {
						return ErrBPSRequestAuthorizationRevoked
					}
					return nil
				})
			}
			result, err := resolver.Resolve(ctx, plan, managedInlineTestAccount(server.URL), body)
			require.Nil(t, result)
			if phase == "missing authority" {
				require.ErrorIs(t, err, ErrBPSRequestAuthorizationUnavailable)
			} else {
				require.ErrorIs(t, err, ErrBPSRequestAuthorizationRevoked)
			}
			if phase == "missing authority" || phase == "revoked before describe" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
			require.Empty(t, ledger.inlineStarts)
			require.Empty(t, ledger.inlineCompletes)
		})
	}
}
