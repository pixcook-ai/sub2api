package service

import (
	"context"
	"fmt"
	"net/http"
)

// Diagnostics stay on HTTP to measure that route, but share the normal
// OAuth outbound transport with business traffic.
type codexProbePreparedKey struct{}

func (s *OpenAIGatewayService) doOpenAICodexProbe(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s == nil || s.httpUpstream == nil || req == nil || account == nil {
		return nil, fmt.Errorf("codex probe transport is unavailable")
	}
	if err := s.prepareOpenAICodexProbeRequest(req, account); err != nil {
		return nil, err
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(req.Context(), req, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
}

func (s *OpenAIGatewayService) prepareOpenAICodexProbeRequest(req *http.Request, account *Account) error {
	if account == nil || req == nil {
		return nil
	}
	if prepared, _ := req.Context().Value(codexProbePreparedKey{}).(bool); prepared {
		return nil
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	setOpenAIChatGPTAccountHeaders(req.Header, account)
	account.ApplyHeaderOverrides(req.Header)
	*req = *req.WithContext(context.WithValue(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI), codexProbePreparedKey{}, true))
	return nil
}
