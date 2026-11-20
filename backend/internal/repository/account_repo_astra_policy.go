package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Called in the UpdateExtra transaction only when a probe writes its history.
// Row locking serializes different processes without a schema migration.
func lockAstraPolicyExtraUpdate(ctx context.Context, client *dbent.Client, id int64, updates map[string]any) (map[string]any, error) {
	rows, err := client.QueryContext(ctx, "SELECT extra -> $1 FROM accounts WHERE id = $2 AND deleted_at IS NULL FOR NO KEY UPDATE", service.OpenAIAstraPolicyExtraKey, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrAccountNotFound
	}
	var data []byte
	if err := rows.Scan(&data); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var current any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &current); err != nil {
			return nil, fmt.Errorf("decode persisted Astra history: %w", err)
		}
	}
	_, previousAt := service.LatestOpenAIAstraSample(current)
	_, incomingAt := service.LatestOpenAIAstraSample(updates[service.OpenAIAstraPolicyExtraKey])
	if previousAt.After(incomingAt) {
		updates = maps.Clone(updates)
		delete(updates, service.OpenAIAstraPolicyExtraKey)
	}
	return updates, nil
}

// The caller already holds the account row lock acquired by
// lockAndMergeAccountProbeExtra. Reading here avoids extending its established
// projection and keeps stale admin forms from overwriting runtime probe state.
func mergeLockedTiboRuntimeExtra(ctx context.Context, client *dbent.Client, id int64, incoming map[string]any) (map[string]any, error) {
	var raw []byte
	if err := scanSingleRow(ctx, client, "SELECT extra FROM accounts WHERE id=$1 AND deleted_at IS NULL", []any{id}, &raw); err != nil {
		return nil, err
	}
	var current map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, err
		}
	}
	return service.MergeOpenAITiboExtra(incoming, current), nil
}
