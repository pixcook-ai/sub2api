package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// GetOpenAITiboQualityIsolation reads only the durable account/parent quality
// bits at final admission, independently of whether monitoring is enabled.
// It does not load credentials, proxy, groups or other account associations.
func (r *accountRepository) GetOpenAITiboQualityIsolation(ctx context.Context, id int64) (bool, error) {
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		return scoped.GetOpenAITiboQualityIsolation(ctx, id)
	}
	var isolated, missingParent bool
	err := scanSingleRow(ctx, r.sql, openAITiboQualityIsolationSQL, []any{id}, &isolated, &missingParent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrAccountNotFound
	}
	if err != nil {
		return false, err
	}
	if !isolated && missingParent {
		return false, service.ErrAccountNotFound
	}
	return isolated, nil
}

const openAITiboQualityIsolationSQL = `SELECT
 COALESCE(a.platform = 'openai' AND (
  COALESCE(a.extra ->> 'openai_tibo_quality_isolated', 'false') = 'true'
  OR (p.platform = 'openai' AND COALESCE(p.extra ->> 'openai_tibo_quality_isolated', 'false') = 'true')
 ), false) AS isolated,
 a.parent_account_id IS NOT NULL AND p.id IS NULL AS missing_parent
FROM accounts a
LEFT JOIN accounts p ON p.id = a.parent_account_id AND p.deleted_at IS NULL
WHERE a.id = $1 AND a.deleted_at IS NULL`
