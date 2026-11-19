package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const intelligentCaptureTextLimit = 512 << 10

type intelligentCompletedOutput struct {
	items    map[int]string
	overflow bool
	invalid  bool
}

func (c *intelligentCompletedOutput) observe(event map[string]any) {
	kind, _ := event["type"].(string)
	if kind != "response.output_item.added" && kind != "response.output_item.done" {
		return
	}
	item, _ := event["item"].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		return
	}
	index, ok := event["output_index"].(float64)
	if !ok || index < 0 || index != float64(int(index)) {
		c.invalid = true
		return
	}
	if kind == "response.output_item.done" {
		if c.items == nil {
			c.items = make(map[int]string)
		}
		text, _ := intelligentResponsesTerminalText(map[string]any{"response": map[string]any{"output": []any{item}}})
		c.items[int(index)] = text
	}
}

func (c *intelligentCompletedOutput) terminalText(event map[string]any) (string, bool, bool) {
	text, present := intelligentResponsesTerminalText(event)
	response, _ := event["response"].(map[string]any)
	output, isArray := response["output"].([]any)
	if !isArray || len(output) != 0 || c.invalid || c.overflow || len(c.items) == 0 {
		return text, present, c.overflow
	}
	positions := make([]int, 0, len(c.items))
	for position := range c.items {
		positions = append(positions, position)
	}
	sort.Ints(positions)
	var result strings.Builder
	for _, position := range positions {
		_, _ = result.WriteString(c.items[position])
	}
	return result.String(), true, result.Len() > intelligentCaptureTextLimit
}

func intelligentResponsesTerminalSuccessful(event map[string]any) bool {
	kind, _ := event["type"].(string)
	if kind != "response.completed" && kind != "response.done" {
		return false
	}
	if event["error"] != nil {
		return false
	}
	response, _ := event["response"].(map[string]any)
	if response["error"] != nil {
		return false
	}
	return true
}

func intelligentResponsesTerminalText(event map[string]any) (string, bool) {
	response, _ := event["response"].(map[string]any)
	output, present := response["output"].([]any)
	if !present {
		return "", false
	}
	var text strings.Builder
	for _, value := range output {
		item, _ := value.(map[string]any)
		content, _ := item["content"].([]any)
		for _, value := range content {
			part, _ := value.(map[string]any)
			if part["type"] == "output_text" {
				_, _ = text.WriteString(fmt.Sprint(part["text"]))
			}
		}
	}
	if text.Len() > intelligentCaptureTextLimit {
		return text.String()[:intelligentCaptureTextLimit], true
	}
	return text.String(), true
}

// openAITestResponseAuthorization extracts the Authorization header from the
// final outbound request, verifying that the workspace matches this account.
// Used by the access probe to confirm the exact credential that was sent.
func openAITestResponseAuthorization(account *Account, sent *http.Request) string {
	if sent == nil || sent.URL == nil || sent.Method != http.MethodPost || sent.URL.String() != chatgptCodexAPIURL {
		return ""
	}
	if sent.Host != "" && !strings.EqualFold(sent.Host, sent.URL.Host) {
		return ""
	}
	workspace, ok := openAITestIdentityHeader(sent.Header, "chatgpt-account-id")
	if !ok || workspace != account.GetChatGPTAccountID() {
		return ""
	}
	authorization, ok := openAITestIdentityHeader(sent.Header, "Authorization")
	if !ok {
		return ""
	}
	return authorization
}

// openAITestIdentityHeader returns a header value only when it appears exactly
// once and under a single canonical casing. Ambiguous headers are rejected.
func openAITestIdentityHeader(headers http.Header, name string) (string, bool) {
	var value string
	found := false
	for key, values := range headers {
		if !strings.EqualFold(name, key) {
			continue
		}
		if found || len(values) != 1 {
			return "", false
		}
		value, found = values[0], true
	}
	return value, true
}

// readOpenAIAccountTestStream reads an OpenAI SSE stream and returns the final
// text output. onDelta is called for each incremental text delta when non-nil.
func readOpenAIAccountTestStream(body io.Reader, onDelta func(string)) (string, error) {
	reader := bufio.NewReader(body)
	var text strings.Builder
	var completedOutput intelligentCompletedOutput
	terminalError := func(data map[string]any, eventType string) error {
		response, _ := data["response"].(map[string]any)
		for _, value := range []any{response["error"], data["error"]} {
			if errData, ok := value.(map[string]any); ok {
				if message, ok := errData["message"].(string); ok && message != "" {
					return errors.New(message)
				}
			}
		}
		message := "OpenAI response was not completed successfully"
		if eventType != "response.completed" && eventType != "response.done" {
			message = "OpenAI response " + strings.TrimPrefix(eventType, "response.")
		}
		if details, ok := response["incomplete_details"].(map[string]any); ok {
			if reason, ok := details["reason"].(string); ok && reason != "" {
				message += ": " + reason
			}
		}
		return errors.New(message)
	}

	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" && sseDataPrefix.MatchString(line) {
			jsonStr := sseDataPrefix.ReplaceAllString(line, "")
			if jsonStr == "[DONE]" {
				return "", errors.New("Stream ended before response.completed")
			}
			var data map[string]any
			if json.Unmarshal([]byte(jsonStr), &data) == nil {
				completedOutput.observe(data)
				eventType, _ := data["type"].(string)
				switch eventType {
				case "response.output_text.delta":
					if delta, ok := data["delta"].(string); ok && delta != "" {
						if len(delta) > intelligentCaptureTextLimit-text.Len() {
							return "", errors.New("Account test output is too large")
						}
						_, _ = text.WriteString(delta)
						if onDelta != nil {
							onDelta(delta)
						}
					}
				case "response.completed", "response.done":
					if !intelligentResponsesTerminalSuccessful(data) {
						return "", terminalError(data, eventType)
					}
					finalText, present, truncated := completedOutput.terminalText(data)
					if !present {
						finalText = text.String()
					}
					if truncated || len(finalText) > intelligentCaptureTextLimit {
						return "", errors.New("Account test output is too large")
					}
					if onDelta != nil {
						if !strings.HasPrefix(finalText, text.String()) {
							return "", errors.New("OpenAI final output differs from the streamed text")
						}
						finalText = finalText[text.Len():]
					}
					return finalText, nil
				case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
					return "", terminalError(data, eventType)
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return "", errors.New("Stream ended before response.completed")
			}
			return "", fmt.Errorf("Stream read error: %w", readErr)
		}
	}
}

// codexProbeGateway returns a minimal OpenAIGatewayService for use by the
// access probe. It reuses the test service's HTTP upstream and account repo.
func (s *AccountTestService) codexProbeGateway(plugins *PluginManager) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg: s.cfg, httpUpstream: s.httpUpstream,
		accountRepo: s.accountRepo, pluginManager: plugins,
	}
}
