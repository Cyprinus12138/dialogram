package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPCompleter is a minimal, tool-free, non-streaming single-shot client
// against an OpenAI-compatible /chat/completions endpoint. It exists only
// for the judge paths; nothing here runs in the zero-cost tiers. It
// deliberately does not reuse the agent loop: a judge wants a bare
// completion with a custom system prompt and no tool registry.
type HTTPCompleter struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string
	Client  *http.Client // optional; defaults to a 60s client

	// MaxAttempts bounds retries on transient upstream failures (429 / 5xx
	// / network). Defaults to 3.
	MaxAttempts int
	// Backoff is the base delay between attempts; it doubles each time.
	// Defaults to 3s. Tests set a tiny value.
	Backoff time.Duration
	// OmitTemperature drops the `temperature` field. Some compatible
	// endpoints reject it for newer models.
	OmitTemperature bool
}

const (
	defaultMaxAttempts = 3
	defaultBackoff     = 3 * time.Second
)

// TransientError marks a completion that failed on a retryable upstream
// condition even after retries were exhausted. Runners classify it as
// "could not obtain a judgment" (infra) rather than a rubric regression.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return "transient judge failure: " + e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Complete sends one system+user turn and returns the assistant text.
// Transient failures are retried with exponential backoff; if they persist
// the error is a *TransientError. Permanent failures (bad key, 400) surface
// immediately and unwrapped.
func (c *HTTPCompleter) Complete(ctx context.Context, system, user string) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("HTTPCompleter: APIKey not set")
	}
	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}
	base := c.Backoff
	if base <= 0 {
		base = defaultBackoff
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		out, retryable, err := c.complete1(ctx, system, user)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
		if attempt < attempts {
			delay := base << (attempt - 1)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return "", &TransientError{Err: lastErr}
}

func (c *HTTPCompleter) complete1(ctx context.Context, system, user string) (out string, retryable bool, err error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := c.Model
	if model == "" {
		model = "gpt-4o-mini"
	}
	cl := c.Client
	if cl == nil {
		cl = &http.Client{Timeout: 60 * time.Second}
	}
	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		// Judges should be deterministic; a low temperature reduces score
		// jitter run to run.
		"temperature": 0,
		"max_tokens":  1024,
	}
	if c.OmitTemperature {
		delete(reqBody, "temperature")
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("content-type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("judge request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", transient, fmt.Errorf("judge %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", false, fmt.Errorf("judge decode: %w", err)
	}
	if parsed.Error != nil {
		return "", false, fmt.Errorf("judge api error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", false, fmt.Errorf("judge: empty choices")
	}
	return parsed.Choices[0].Message.Content, false, nil
}
