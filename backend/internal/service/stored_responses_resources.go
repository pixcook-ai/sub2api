package service

import (
	"context"
	"errors"
	"strings"
)

func (s *StoredResponseService) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

// GetManagementModel includes retained deletion tombstones so repeated DELETE
// requests can still enforce the current group's original-client-model policy.
func (s *StoredResponseService) GetManagementModel(ctx context.Context, owner StoredResponseOwner, id string) (string, error) {
	if !s.Enabled() {
		return "", ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) || s.repo == nil {
		return "", ErrStoredResponseNotFound
	}
	model, err := s.repo.GetManagementModel(ctx, owner.UserID, owner.GroupID, id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(model) == "" {
		return "", errors.New("stored response management model unavailable")
	}
	return model, nil
}
