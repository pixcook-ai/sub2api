package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

var (
	ErrFileNotFound      = errors.New("file not found")
	ErrFileQuotaExceeded = errors.New("file storage quota exceeded")
	ErrFileLeaseConflict = errors.New("file storage lease lost")
	ErrInvalidFile       = errors.New("invalid file upload")
	ErrFilesDisabled     = errors.New("files storage is disabled")
)

// StoredFile is a local durable resource. It carries no BPS upload or processing status.
type StoredFile struct {
	ID                                                              int64
	PublicID                                                        string
	UserID, APIKeyID, GroupID                                       int64
	StorageKey, Filename, Purpose, MIME, State, UploadToken, SHA256 string
	Bytes, ReservedBytes                                            int64
	CreatedAt                                                       time.Time
}

type FileOwner struct{ UserID, APIKeyID, GroupID int64 }

// FileRepository coordinates budgets and leases across all processes sharing the volume.
type FileRepository interface {
	Reserve(context.Context, *StoredFile, int64, int64, time.Duration) error
	CommitUpload(context.Context, int64, string, int64, string, string) error
	AbortUpload(context.Context, int64, string) (bool, error)
	GetForOwner(context.Context, int64, int64, string) (*StoredFile, error)
	AcquireRead(context.Context, int64, int64, string, string, time.Duration) (*StoredFile, error)
	RenewUpload(context.Context, int64, string, time.Duration) error
	RenewRead(context.Context, int64, string, time.Duration) error
	ReleaseRead(context.Context, int64, string) error
	ClaimStaleUploads(context.Context, string, time.Duration, int) ([]*StoredFile, error)
	FinishCleanup(context.Context, int64, string) error
}

type FileBlobStore interface {
	Stage(context.Context, string, io.Reader, int64) (int64, string, error)
	Commit(context.Context, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Remove(context.Context, string) error
	CleanupStaging(context.Context, string) error
	FreeBytes(context.Context) (int64, error)
}

type FileService struct {
	repo  FileRepository
	store FileBlobStore
	cfg   config.FilesConfig
}

func NewFileService(repo FileRepository, store FileBlobStore, cfg config.FilesConfig) (*FileService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Enabled && (repo == nil || store == nil) {
		return nil, errors.New("enabled files require repository and private storage")
	}
	return &FileService{repo: repo, store: store, cfg: cfg}, nil
}

// ProvideFileService leaves the feature inert unless an explicit volume is configured.
func ProvideFileService(repo FileRepository, cfg *config.Config) (*FileService, error) {
	if !cfg.Gateway.Files.Enabled {
		return NewFileService(repo, nil, cfg.Gateway.Files)
	}
	store, err := NewPrivateFileBlobStore(cfg.Gateway.Files.Root, cfg.Gateway.Files.FreeSpaceReserveBytes)
	if err != nil {
		return nil, err
	}
	svc, err := NewFileService(repo, store, cfg.Gateway.Files)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = svc.Recover(ctx); err != nil {
		return nil, fmt.Errorf("recover private file staging: %w", err)
	}
	return svc, nil
}

func (s *FileService) Enabled() bool             { return s != nil && s.cfg.Enabled }
func (s *FileService) MaxUploadBodyBytes() int64 { return s.cfg.MaxFileBytes + 64*1024 }
func (s *FileService) lease() time.Duration      { return time.Duration(s.cfg.LeaseSeconds) * time.Second }

func fileToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// UploadMultipart streams exactly one file; fields may appear before or after it.
// The full max-file reservation is made before disk IO, including unknown-length uploads.
func (s *FileService) UploadMultipart(ctx context.Context, owner FileOwner, reader *multipart.Reader) (_ *StoredFile, retErr error) {
	if !s.Enabled() {
		return nil, ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || owner.GroupID <= 0 {
		return nil, ErrInvalidFile
	}
	// Revisit uploads whose old process lease was still live during startup.
	// This is staging recovery only; it does not expire completed Files.
	if err := s.Recover(ctx); err != nil {
		return nil, err
	}
	var record *StoredFile
	var purpose string
	var purposeSeen bool
	var size int64
	var hash string
	var stop func() error
	defer func() {
		if stop != nil {
			if err := stop(); retErr == nil && err != nil {
				retErr = err
			}
		}
		if retErr != nil && record != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ok, err := s.repo.AbortUpload(cleanupCtx, record.ID, record.UploadToken)
			if err == nil && ok {
				if s.store.Remove(cleanupCtx, record.StorageKey) == nil {
					_ = s.repo.FinishCleanup(cleanupCtx, record.ID, record.UploadToken)
				}
			}
		}
	}()
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: malformed multipart: %w", ErrInvalidFile, err)
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return nil, ErrInvalidFile
		}
		switch params["name"] {
		case "purpose":
			if purposeSeen || params["filename"] != "" {
				return nil, ErrInvalidFile
			}
			purposeSeen = true
			value, err := io.ReadAll(io.LimitReader(part, 1025))
			if err != nil {
				return nil, err
			}
			if len(value) > 1024 {
				return nil, ErrInvalidFile
			}
			purpose = string(value)
		case "file":
			if record != nil {
				return nil, fmt.Errorf("%w: exactly one file is required", ErrInvalidFile)
			}
			filename := params["filename"]
			contentType := part.Header.Get("Content-Type")
			if err := validateFileMetadata(filename, contentType); err != nil {
				return nil, err
			}
			key, err := fileToken()
			if err != nil {
				return nil, err
			}
			token, err := fileToken()
			if err != nil {
				return nil, err
			}
			record = &StoredFile{PublicID: "file-" + key, StorageKey: key, Filename: filename, MIME: contentType, UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, UploadToken: token, ReservedBytes: s.cfg.MaxFileBytes, State: "staging"}
			if err = s.repo.Reserve(ctx, record, s.cfg.UserQuotaBytes, s.cfg.GlobalQuotaBytes, s.lease()); err != nil {
				// Commit acknowledgement may be lost after PostgreSQL persisted the
				// reservation. Retain an assigned ID for token-guarded cleanup.
				if record.ID == 0 {
					record = nil
				}
				return nil, err
			}
			var uploadCtx context.Context
			uploadCtx, stop = s.keepLease(ctx, func(c context.Context) error { return s.repo.RenewUpload(c, record.ID, record.UploadToken, s.lease()) })
			size, hash, err = s.store.Stage(uploadCtx, key, part, s.cfg.MaxFileBytes)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: unsupported multipart field", ErrInvalidFile)
		}
		if err := part.Close(); err != nil {
			return nil, err
		}
	}
	if record == nil || size == 0 || !validFilePurpose(purpose) {
		return nil, fmt.Errorf("%w: a nonempty file and supported purpose are required", ErrInvalidFile)
	}
	if err := stop(); err != nil {
		stop = nil
		return nil, err
	}
	stop = nil
	if err := s.repo.RenewUpload(ctx, record.ID, record.UploadToken, s.lease()); err != nil {
		return nil, err
	}
	if err := s.store.Commit(ctx, record.StorageKey); err != nil {
		return nil, err
	}
	if err := s.repo.CommitUpload(ctx, record.ID, record.UploadToken, size, hash, purpose); err != nil {
		return nil, err
	}
	record.Bytes, record.ReservedBytes, record.SHA256, record.Purpose, record.State = size, size, hash, purpose, "ready"
	return record, nil
}

func validateFileMetadata(filename, contentType string) error {
	if filename == "" || len(filename) > 255 || !utf8.ValidString(filename) || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("%w: unsafe filename", ErrInvalidFile)
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: unsafe filename", ErrInvalidFile)
		}
	}
	if filepath.Base(filename) != filename {
		return ErrInvalidFile
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("%w: invalid content type", ErrInvalidFile)
	}
	switch mt {
	case "application/octet-stream", "application/pdf", "application/json", "application/zip", "application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "text/plain", "text/csv", "text/markdown", "image/png", "image/jpeg", "image/webp", "image/gif":
		return nil
	default:
		return fmt.Errorf("%w: unsupported content type", ErrInvalidFile)
	}
}

func validFilePurpose(p string) bool {
	switch p {
	case "user_data", "assistants", "batch", "fine-tune", "vision", "evals":
		return true
	}
	return false
}

func (s *FileService) Get(ctx context.Context, owner FileOwner, id string) (*StoredFile, error) {
	if !s.Enabled() {
		return nil, ErrFilesDisabled
	}
	return s.repo.GetForOwner(ctx, owner.UserID, owner.GroupID, id)
}

func (s *FileService) Open(ctx context.Context, owner FileOwner, id string) (*StoredFile, io.ReadCloser, error) {
	if !s.Enabled() {
		return nil, nil, ErrFilesDisabled
	}
	token, err := fileToken()
	if err != nil {
		return nil, nil, err
	}
	record, err := s.repo.AcquireRead(ctx, owner.UserID, owner.GroupID, id, token, s.lease())
	if err != nil {
		return nil, nil, err
	}
	readCtx, stop := s.keepLease(ctx, func(c context.Context) error { return s.repo.RenewRead(c, record.ID, token, s.lease()) })
	release := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.repo.ReleaseRead(cleanupCtx, record.ID, token)
	}
	reader, err := s.store.Open(readCtx, record.StorageKey)
	if err != nil {
		_ = stop()
		release()
		return nil, nil, err
	}
	return record, &leasedFileReader{ReadCloser: reader, ctx: readCtx, stop: stop, release: release}, nil
}

// Recover only reclaims expired upload/cleanup leases, never active uploads or reads.
// A crash after rename but before DB commit is treated as an incomplete local upload.
func (s *FileService) Recover(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	token, err := fileToken()
	if err != nil {
		return err
	}
	for {
		records, err := s.repo.ClaimStaleUploads(ctx, token, s.lease(), 100)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err = s.store.Remove(ctx, record.StorageKey); err != nil {
				return err
			}
			if err = s.repo.FinishCleanup(ctx, record.ID, token); err != nil {
				return err
			}
		}
		if len(records) < 100 {
			return nil
		}
	}
}

func (s *FileService) keepLease(parent context.Context, renew func(context.Context) error) (context.Context, func() error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	stop := make(chan struct{})
	var once sync.Once
	var leaseErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.lease() / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, s.lease()/6)
				err := renew(renewCtx)
				renewCancel()
				if err != nil {
					leaseErr = err
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() error { once.Do(func() { close(stop); <-done; cancel() }); return leaseErr }
}

type leasedFileReader struct {
	io.ReadCloser
	ctx      context.Context
	stop     func() error
	release  func()
	once     sync.Once
	closeErr error
}

func (r *leasedFileReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(p)
}
func (r *leasedFileReader) Close() error {
	r.once.Do(func() {
		r.closeErr = r.ReadCloser.Close()
		if err := r.stop(); r.closeErr == nil {
			r.closeErr = err
		}
		r.release()
	})
	return r.closeErr
}
