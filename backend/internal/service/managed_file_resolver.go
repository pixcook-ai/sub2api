package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/managedfiles"
	"io"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const managedFileReservationLease = 15 * time.Minute

type ManagedFileResolver struct {
	files    *FileService
	ledger   ManagedFileRepository
	upstream HTTPUpstream
	cfg      *config.Config
}

func NewManagedFileResolver(files *FileService, ledger ManagedFileRepository, upstream HTTPUpstream, cfg *config.Config) *ManagedFileResolver {
	return &ManagedFileResolver{files: files, ledger: ledger, upstream: upstream, cfg: cfg}
}
func (s *ManagedFileResolver) Enabled() bool { return s != nil && s.files.Enabled() && s.ledger != nil }

type managedFilePlanKey struct{}

func WithManagedFilePlan(ctx context.Context, plan *ManagedFilePlan) context.Context {
	return context.WithValue(ctx, managedFilePlanKey{}, plan)
}
func ManagedFilePlanFromContext(ctx context.Context) *ManagedFilePlan {
	plan, _ := ctx.Value(managedFilePlanKey{}).(*ManagedFilePlan)
	return plan
}

type ManagedFilePlan struct {
	RequiredAccountID int64
	owner             FileOwner
	refs              []ManagedFileReference
	files             []*StoredFile
	readers           map[string]io.ReadCloser
	resolver          *ManagedFileResolver
	reservation       *ManagedFileReservation
	mu                sync.Mutex
	closed            bool
	resolving         bool
	once              sync.Once
	closeErr          error
}

func (p *ManagedFilePlan) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.once.Do(func() {
		p.closed = true
		for _, reader := range p.readers {
			p.closeErr = errors.Join(p.closeErr, reader.Close())
		}
		if p.reservation != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p.closeErr = errors.Join(p.closeErr, p.resolver.ledger.ReleaseReservation(ctx, p.reservation))
		}
	})
	return p.closeErr
}
func (s *ManagedFileResolver) Prepare(ctx context.Context, owner FileOwner, body []byte) (_ *ManagedFilePlan, retErr error) {
	refs, err := CollectManagedResponseFiles(body)
	if err != nil {
		return nil, err
	}
	if len(refs) > managedfiles.MaxUniqueFiles {
		return nil, ErrManagedFileUnsupportedInput
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if !s.Enabled() {
		return nil, ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.GroupID <= 0 || owner.APIKeyID <= 0 {
		return nil, ErrInvalidFile
	}
	plan := &ManagedFilePlan{owner: owner, refs: refs, readers: make(map[string]io.ReadCloser), resolver: s}
	defer func() {
		if retErr != nil {
			_ = plan.Close()
		}
	}()
	ids := make([]string, 0, len(refs))
	type messageBudget struct {
		positions int
		bytes     int64
	}
	budgets := make(map[int]messageBudget)
	for _, ref := range refs {
		f, err := s.files.Get(ctx, owner, ref.PublicID)
		if err != nil {
			return nil, err
		}
		if !managedResolverReady(f, owner) {
			return nil, ErrFileNotFound
		}
		if f.Bytes <= 0 || f.Bytes > managedfiles.MaxFileBytes {
			return nil, ErrInvalidFile
		}
		for _, position := range ref.Positions {
			b := budgets[position.Input]
			if b.positions >= managedfiles.MaxMessagePositions || f.Bytes > managedfiles.MaxMessageBytes-b.bytes {
				return nil, ErrManagedFileUnsupportedInput
			}
			b.positions++
			b.bytes += f.Bytes
			budgets[position.Input] = b
		}
		ids = append(ids, ref.PublicID)
	}
	lookup, err := s.ledger.LookupBindings(ctx, owner, ids)
	if err != nil {
		return nil, err
	}
	plan.RequiredAccountID = lookup.AccountID
	for _, ref := range refs {
		f, reader, err := s.files.Open(ctx, owner, ref.PublicID)
		if err != nil {
			return nil, err
		}
		plan.readers[ref.PublicID] = reader
		if !managedResolverReady(f, owner) {
			return nil, ErrFileNotFound
		}
		plan.files = append(plan.files, f)
	}
	return plan, nil
}
func managedResolverReady(f *StoredFile, owner FileOwner) bool {
	return f != nil && f.UserID == owner.UserID && f.GroupID == owner.GroupID && f.State == "ready" && f.RetiredAt == nil && f.LocalPurgedAt == nil && (f.ExpiresAt == nil || time.Now().Before(*f.ExpiresAt))
}

type ManagedFileResolved struct {
	Body                                           []byte
	AdmissionID, OwnerRef, GroupRef, BindingSHA256 string
	AccountID                                      int64
	Config                                         BPSFilesBackendConfig
	authorize                                      func(context.Context) error
}

func (r *ManagedFileResolved) Authorize(ctx context.Context) error {
	if r == nil || r.authorize == nil {
		return ErrManagedFileNotReady
	}
	return r.authorize(ctx)
}
func (s *ManagedFileResolver) Resolve(ctx context.Context, plan *ManagedFilePlan, account *Account, finalBody []byte) (*ManagedFileResolved, error) {
	if !s.Enabled() || plan == nil || plan.resolver != s {
		return nil, ErrManagedFileNotReady
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.closed || plan.resolving {
		return nil, ErrManagedFileNotReady
	}
	plan.resolving = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refs, err := CollectManagedResponseFiles(finalBody)
	if err != nil || !reflect.DeepEqual(refs, plan.refs) {
		return nil, ErrManagedFileUnsupportedInput
	}
	backendConfig, err := BPSFilesConfigForAccount(account)
	if err != nil {
		return nil, ErrManagedFileNotReady
	}
	if plan.RequiredAccountID != 0 && plan.RequiredAccountID != account.ID {
		return nil, ErrManagedFileBindingConflict
	}
	backend, err := NewBPSFilesBackend(backendConfig, s.upstream, s.cfg)
	if err != nil {
		return nil, err
	}
	resolveCtx, cancel := context.WithTimeout(ctx, managedFileReservationLease)
	defer cancel()
	description, err := backend.Describe(resolveCtx)
	if err != nil {
		return nil, err
	}
	token, err := fileToken()
	if err != nil {
		return nil, err
	}
	requestID, err := fileToken()
	if err != nil {
		return nil, err
	}
	versions := make([]ManagedFileVersion, 0, len(plan.files))
	for _, file := range plan.files {
		versions = append(versions, ManagedFileVersion{PublicID: file.PublicID, Version: file.Version})
	}
	reservation, err := s.ledger.Reserve(resolveCtx, ManagedFileReserveInput{Owner: plan.owner, Files: versions, Selection: ManagedFileSelection{AccountID: account.ID, Identity: backendConfig.Expected}, LeaseToken: token, Lease: managedFileReservationLease})
	if err != nil {
		return nil, err
	}
	plan.reservation = reservation
	if reservation == nil || reservation.Owner != plan.owner || reservation.Selection != (ManagedFileSelection{AccountID: account.ID, Identity: backendConfig.Expected}) || len(reservation.Files) != len(plan.files) {
		return nil, ErrManagedFileNotReady
	}
	bindings := make(map[string]ManagedFileBinding, len(reservation.Files))
	for _, binding := range reservation.Files {
		if binding.File == nil || bindings[binding.File.PublicID].File != nil {
			return nil, ErrManagedFileNotReady
		}
		bindings[binding.File.PublicID] = binding
	}
	manifest := managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: requestID, Expected: backendConfig.Expected, OwnerRef: strconv.FormatInt(plan.owner.UserID, 10), GroupRef: strconv.FormatInt(plan.owner.GroupID, 10), RequestTemplate: append([]byte(nil), finalBody...)}
	readers := make(map[string]io.Reader)
	var total int64
	for i, file := range plan.files {
		binding, ok := bindings[file.PublicID]
		if !ok || binding.File.Version != file.Version || binding.File.Bytes != file.Bytes || binding.File.SHA256 != file.SHA256 || binding.File.Filename != file.Filename || binding.File.MIME != file.MIME || binding.Selection != reservation.Selection || binding.Operation.ID == "" {
			return nil, ErrManagedFileNotReady
		}
		f := managedfiles.File{FileRef: file.PublicID, Filename: file.Filename, MIME: file.MIME, Bytes: file.Bytes, SHA256: file.SHA256, Positions: plan.refs[i].Positions}
		switch binding.Operation.State {
		case "reserved":
			f.Mode = "upload"
			f.Part = fmt.Sprintf("file_%d", i)
			readers[file.PublicID] = plan.readers[file.PublicID]
			total += file.Bytes
		case "succeeded":
			f.Mode = "existing"
			f.RemoteID = binding.Operation.RemoteID
			f.OperationID = binding.Operation.ID
		case "started", "uncertain":
			return nil, ErrManagedFileUncertain
		default:
			return nil, ErrManagedFileNotReady
		}
		manifest.Files = append(manifest.Files, f)
	}
	if err := managedfiles.ValidateManifest(manifest); err != nil {
		return nil, err
	}
	limits := description.Limits
	encodedManifest, err := json.Marshal(manifest)
	if err != nil {
		return nil, ErrManagedFileUnsupportedInput
	}
	if len(manifest.Files) > limits.UniqueFiles || int64(len(encodedManifest)) > limits.ManifestBytes || total > limits.AdmissionBytes {
		return nil, bpsFilesError("admission_too_large")
	}
	type budget struct {
		positions int
		bytes     int64
	}
	budgets := map[int]budget{}
	for _, f := range manifest.Files {
		if f.Bytes > limits.FileBytes {
			return nil, bpsFilesError("file_too_large")
		}
		for _, position := range f.Positions {
			b := budgets[position.Input]
			if b.positions >= limits.MessagePositions || f.Bytes > limits.MessageBytes-b.bytes {
				return nil, bpsFilesError("message_too_large")
			}
			b.positions++
			b.bytes += f.Bytes
			budgets[position.Input] = b
		}
	}
	admission, err := backend.Admit(resolveCtx, manifest, readers)
	if err != nil {
		return nil, err
	}
	expires := time.Unix(admission.ExpiresAt, 0)
	results := make([]managedfiles.EnsureResult, 0, len(readers))
	ready := admission.Ready
	for _, file := range manifest.Files {
		if file.Mode != "upload" {
			continue
		}
		if err := resolveCtx.Err(); err != nil {
			return nil, err
		}
		if !time.Now().Before(expires) {
			return nil, ErrManagedFileNotReady
		}
		binding := bindings[file.FileRef]
		if err := s.ledger.StartOperation(resolveCtx, reservation, file.FileRef); err != nil {
			return nil, err
		}
		result, err := backend.EnsureUploaded(resolveCtx, managedfiles.EnsureRequest{Contract: managedfiles.Contract, Expected: backendConfig.Expected, AdmissionID: admission.AdmissionID, FileRef: file.FileRef, OperationID: binding.Operation.ID}, file)
		if err != nil {
			s.markUncertain(reservation, file.FileRef)
			return nil, err
		}
		// Preserve a confirmed remote copy even when caller cancellation or a late
		// deadline would otherwise prevent this local acknowledgement from committing.
		persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
		active, completeErr := s.ledger.CompleteOperation(persistCtx, reservation, file.FileRef, result)
		persistCancel()
		if completeErr != nil {
			s.markUncertain(reservation, file.FileRef)
			return nil, completeErr
		}
		if !active {
			return nil, ErrFileLeaseConflict
		}
		results = append(results, result)
		ready = result.Ready
	}
	if !ready {
		return nil, ErrManagedFileNotReady
	}
	bound, err := managedfiles.Bind(manifest, results)
	if err != nil {
		return nil, err
	}
	if admission.Ready && (admission.WireSHA256 != bound.WireSHA256 || admission.BindingSHA256 != bound.BindingSHA256) {
		return nil, ErrManagedFileNotReady
	}
	if err := resolveCtx.Err(); err != nil {
		return nil, err
	}
	resolved := &ManagedFileResolved{Body: bound.Wire, AdmissionID: admission.AdmissionID, OwnerRef: manifest.OwnerRef, GroupRef: manifest.GroupRef, BindingSHA256: bound.BindingSHA256, AccountID: account.ID, Config: backendConfig}
	resolved.authorize = func(c context.Context) error {
		plan.mu.Lock()
		defer plan.mu.Unlock()
		if plan.closed || !time.Now().Before(expires) {
			return ErrManagedFileNotReady
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return s.ledger.AuthorizeGeneration(c, reservation)
	}
	// The transport repeats this check immediately before the one external send.
	if !time.Now().Before(expires) {
		return nil, ErrManagedFileNotReady
	}
	if err := s.ledger.AuthorizeGeneration(resolveCtx, reservation); err != nil {
		return nil, err
	}
	return resolved, nil
}
func (s *ManagedFileResolver) markUncertain(reservation *ManagedFileReservation, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.ledger.MarkUncertain(ctx, reservation, ref, "upload_outcome_unknown")
}
