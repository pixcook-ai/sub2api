package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ListVisibleFiles uses the original immutable (created_at,id) cursor even
// after retirement or expiry. Cursor authorization is the same as page scope.
func (r *fileRepository) ListVisibleFiles(ctx context.Context, owner service.FileOwner, q service.FileListQuery) (service.FileListPage, error) {
	page := service.FileListPage{Files: make([]*service.StoredFile, 0)}
	if q.Limit < 1 || q.Limit > 10000 || (q.Order != "asc" && q.Order != "desc") {
		return page, service.ErrInvalidFile
	}
	var cursorTime time.Time
	var cursorID int64
	if q.After != "" {
		err := r.db.QueryRowContext(ctx, `SELECT created_at,id FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND ($4='' OR purpose=$4)`,
			q.After, owner.UserID, owner.GroupID, q.Purpose).Scan(&cursorTime, &cursorID)
		if errors.Is(err, sql.ErrNoRows) {
			return page, service.ErrFileInvalidCursor
		}
		if err != nil {
			return page, err
		}
	}
	// Direction is chosen only from these two constant branches, never from
	// caller-provided SQL. All expiry checks share this statement's DB clock.
	comparison, direction := "<", "DESC"
	if q.Order == "asc" {
		comparison, direction = ">", "ASC"
	}
	rows, err := r.db.QueryContext(ctx, `WITH bounds AS MATERIALIZED (SELECT statement_timestamp() AS as_of)
 SELECT `+fileColumns+` FROM gateway_files CROSS JOIN bounds
 WHERE user_id=$1 AND group_id=$2 AND state='ready'
 AND (expires_at IS NULL OR expires_at>bounds.as_of) AND ($3='' OR purpose=$3)
 AND ($4='' OR (created_at,id) `+comparison+` ($5::timestamptz,$6::bigint))
 ORDER BY created_at `+direction+`,id `+direction+` LIMIT $7`,
		owner.UserID, owner.GroupID, q.Purpose, q.After, cursorTime, cursorID, q.Limit+1)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		file, err := scanStoredFile(rows)
		if err != nil {
			return page, err
		}
		page.Files = append(page.Files, file)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Files) > q.Limit {
		page.HasMore = true
		page.Files = page.Files[:q.Limit]
	}
	return page, nil
}
