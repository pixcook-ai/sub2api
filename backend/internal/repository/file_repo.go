package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type fileRepository struct{ db *sql.DB }

func NewFileRepository(db *sql.DB) service.FileRepository { return &fileRepository{db: db} }

const fileColumns = `id, public_id, user_id, api_key_id, group_id, storage_key, filename, purpose, mime,
 state, bytes, reserved_bytes, sha256, COALESCE(upload_token, ''), created_at`

// All processes take this transaction lock before reserving disk budget. In
// progress and pending-cleanup bytes remain charged until durable cleanup.
const fileBudgetAdvisoryLock int64 = 0x66696c6573627564

func (r *fileRepository) Reserve(ctx context.Context, file *service.StoredFile, userQuota, globalQuota int64, lease time.Duration) error {
	if file == nil || file.ReservedBytes <= 0 || userQuota <= 0 || globalQuota <= 0 || lease <= 0 {
		return fmt.Errorf("invalid Files reservation")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, fileBudgetAdvisoryLock); err != nil {
		return err
	}
	var total, owned int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_bytes),0),
 COALESCE(SUM(reserved_bytes) FILTER (WHERE user_id=$1),0)
 FROM gateway_files WHERE state <> 'deleted'`, file.UserID).Scan(&total, &owned); err != nil {
		return err
	}
	// Subtraction avoids overflow even with a malformed caller-supplied size.
	if file.ReservedBytes > globalQuota || total > globalQuota-file.ReservedBytes ||
		file.ReservedBytes > userQuota || owned > userQuota-file.ReservedBytes {
		return service.ErrFileQuotaExceeded
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO gateway_files
 (public_id,user_id,api_key_id,group_id,storage_key,filename,purpose,mime,state,reserved_bytes,upload_token,upload_lease_until)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'staging',$9,$10,clock_timestamp()+make_interval(secs=>$11::double precision))
 RETURNING id,created_at`, file.PublicID, file.UserID, file.APIKeyID, file.GroupID, file.StorageKey, file.Filename, file.Purpose, file.MIME,
		file.ReservedBytes, file.UploadToken, lease.Seconds()).Scan(&file.ID, &file.CreatedAt)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	file.State = "staging"
	return nil
}

func (r *fileRepository) CommitUpload(ctx context.Context, id int64, token string, size int64, sha, purpose string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files SET state='ready',bytes=$3,reserved_bytes=$3,
 sha256=$4,purpose=$5,upload_token=NULL,upload_lease_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND state='staging' AND upload_token=$2 AND upload_lease_until>clock_timestamp()
 AND $3>=0 AND $3<=reserved_bytes`, id, token, size, sha, purpose)
	return fileCASResult(result, err)
}

func (r *fileRepository) AbortUpload(ctx context.Context, id int64, token string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files SET state='tombstone',updated_at=clock_timestamp()
 WHERE id=$1 AND state='staging' AND upload_token=$2`, id, token)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *fileRepository) GetForOwner(ctx context.Context, userID, groupID int64, publicID string) (*service.StoredFile, error) {
	return scanStoredFile(r.db.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND state='ready'`, publicID, userID, groupID))
}

func (r *fileRepository) AcquireRead(ctx context.Context, userID, groupID int64, publicID, token string, lease time.Duration) (*service.StoredFile, error) {
	if token == "" || lease <= 0 {
		return nil, service.ErrFileLeaseConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	file, err := scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND state='ready' FOR UPDATE`, publicID, userID, groupID))
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_file_read_leases(file_id,token,lease_until)
 VALUES($1,$2,clock_timestamp()+make_interval(secs=>$3::double precision))`, file.ID, token, lease.Seconds())
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return file, nil
}

func (r *fileRepository) RenewUpload(ctx context.Context, id int64, token string, lease time.Duration) error {
	if token == "" || lease <= 0 {
		return service.ErrFileLeaseConflict
	}
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files
 SET upload_lease_until=clock_timestamp()+make_interval(secs=>$3::double precision)
 WHERE id=$1 AND upload_token=$2 AND state='staging' AND upload_lease_until>clock_timestamp()`, id, token, lease.Seconds())
	return fileCASResult(result, err)
}

func (r *fileRepository) RenewRead(ctx context.Context, id int64, token string, lease time.Duration) error {
	if token == "" || lease <= 0 {
		return service.ErrFileLeaseConflict
	}
	// Keep the same file->lease lock order as AcquireRead and a future
	// ready->tombstone transition. An MVCC EXISTS alone could race deletion.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM gateway_files WHERE id=$1 AND state='ready' FOR UPDATE`, id).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrFileLeaseConflict
	}
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_file_read_leases SET lease_until=clock_timestamp()+make_interval(secs=>$3::double precision)
 WHERE file_id=$1 AND token=$2 AND lease_until>clock_timestamp()`, id, token, lease.Seconds())
	if err = fileCASResult(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *fileRepository) ReleaseRead(ctx context.Context, id int64, token string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM gateway_file_read_leases WHERE file_id=$1 AND token=$2`, id, token)
	return err
}

func (r *fileRepository) ClaimStaleUploads(ctx context.Context, token string, lease time.Duration, limit int) ([]*service.StoredFile, error) {
	if token == "" || lease <= 0 || limit <= 0 {
		return nil, service.ErrFileLeaseConflict
	}
	rows, err := r.db.QueryContext(ctx, `WITH stale AS (
 SELECT id FROM gateway_files f WHERE state IN ('staging','tombstone') AND upload_lease_until<clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM gateway_file_read_leases l WHERE l.file_id=f.id AND l.lease_until>clock_timestamp())
 ORDER BY upload_lease_until,id FOR UPDATE SKIP LOCKED LIMIT $3
 ) UPDATE gateway_files SET state='tombstone',upload_token=$1,
 upload_lease_until=clock_timestamp()+make_interval(secs=>$2::double precision),updated_at=clock_timestamp()
 WHERE id IN (SELECT id FROM stale) RETURNING `+fileColumns, token, lease.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []*service.StoredFile
	for rows.Next() {
		file, err := scanStoredFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (r *fileRepository) FinishCleanup(ctx context.Context, id int64, token string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files SET state='deleted',reserved_bytes=0,bytes=0,
 upload_token=NULL,upload_lease_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND state='tombstone' AND upload_token=$2
 AND NOT EXISTS(SELECT 1 FROM gateway_file_read_leases WHERE file_id=$1 AND lease_until>clock_timestamp())`, id, token)
	return fileCASResult(result, err)
}

func fileCASResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrFileLeaseConflict
	}
	return nil
}

func scanStoredFile(row interface{ Scan(...any) error }) (*service.StoredFile, error) {
	file := &service.StoredFile{}
	err := row.Scan(&file.ID, &file.PublicID, &file.UserID, &file.APIKeyID, &file.GroupID, &file.StorageKey,
		&file.Filename, &file.Purpose, &file.MIME, &file.State, &file.Bytes, &file.ReservedBytes, &file.SHA256, &file.UploadToken, &file.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrFileNotFound
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}
