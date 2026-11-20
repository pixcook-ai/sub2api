package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type openAITiboPreviousResponseKey struct{}

func withOpenAITiboQualityAdmission(ctx context.Context, body []byte) context.Context {
	previous := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previous == "" {
		return ctx
	}
	return context.WithValue(ctx, openAITiboPreviousResponseKey{}, previous)
}

// checkOpenAITiboQualityAdmission closes the gap between scheduler selection and
// the upstream send (including requests which waited for a slot). Probes use a
// separate transport so an isolated account can still produce recovery samples.
func (s *OpenAIGatewayService) checkOpenAITiboQualityAdmission(ctx context.Context, account *Account, body []byte) error {
	return s.checkOpenAITiboQualityAdmissionForPrevious(ctx, account, gjson.GetBytes(body, "previous_response_id").String())
}

func (s *OpenAIGatewayService) checkOpenAITiboQualityAdmissionForPrevious(ctx context.Context, account *Account, previousResponseID string) error {
	if previousResponseID == "" && ctx != nil {
		previousResponseID, _ = ctx.Value(openAITiboPreviousResponseKey{}).(string)
	}
	if account == nil || !account.IsOpenAI() {
		return nil
	}
	current := account
	if s != nil && s.accountRepo != nil {
		latest, err := s.accountRepo.GetByID(ctx, account.ID)
		if err != nil {
			return fmt.Errorf("verify OpenAI quality isolation: %w", err)
		}
		if latest == nil {
			return fmt.Errorf("verify OpenAI quality isolation: account is unavailable")
		}
		current = latest
	}
	isolated := current.IsOpenAITiboQualityIsolated()
	if !isolated && current.IsShadow() {
		if s == nil || s.accountRepo == nil {
			return fmt.Errorf("verify OpenAI quality isolation: shadow parent is unavailable")
		}
		parent, err := s.accountRepo.GetByID(ctx, *current.ParentAccountID)
		if err != nil || parent == nil {
			return fmt.Errorf("verify OpenAI quality isolation: shadow parent is unavailable")
		}
		isolated = parent.IsOpenAITiboQualityIsolated()
	}
	if !isolated {
		return nil
	}
	action := NextAccountRetry
	if strings.TrimSpace(previousResponseID) != "" {
		// Upstream response state belongs to this account. Never send an unsafe
		// continuation to a different account merely to escape isolation.
		action = NextAccountStop
	}
	return &UpstreamFailoverError{
		StatusCode: http.StatusServiceUnavailable, ClientStatusCode: http.StatusServiceUnavailable,
		ClientMessage: "账号已被 Tibo 质量隔离，停止调度 / Account is isolated by Tibo quality monitoring",
		Reason:        GatewayFailureReason("openai_tibo_quality_isolated"), NextAccountAction: action,
		RequestScopedTransient: true,
	}
}

// A bound response must be rejected before the ordinary resolver discards an
// unschedulable binding and falls back to another account.
func (s *OpenAIGatewayService) checkOpenAITiboPreviousResponseAdmission(ctx context.Context, groupID *int64, previousResponseID string) error {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if s == nil || s.accountRepo == nil || previousResponseID == "" {
		return nil
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return nil
	}
	id, err := store.GetResponseAccount(ctx, derefGroupID(groupID), previousResponseID)
	if err != nil || id <= 0 {
		return nil
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || account == nil {
		// Keep the pre-existing behavior for an unavailable response owner;
		// the ordinary resolver performs its own availability checks.
		return nil
	}
	return s.checkOpenAITiboQualityAdmissionForPrevious(ctx, account, previousResponseID)
}
