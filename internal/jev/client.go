// Package jev is a client for TypeSafe Jev (System One).
// Contract: POST {base}/v1/systemone with state + typed questions.
// It only talks to the new-api gateway, never api.typesafe.ai,
// and never uses chat completions.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jingx8885/evomap-tavern/internal/config"
)

const maxErrorBody = 2048

// Question is one System One question. Type is noul, choice, or score.
type Question struct {
	Type         string         `json:"type"`
	Instructions any            `json:"instructions"`
	Criteria     map[string]any `json:"criteria,omitempty"`
	Levels       []string       `json:"levels,omitempty"`
}

// marshalQuestions maps score Levels onto the protocol criteria array.
func marshalQuestions(qs map[string]Question) (map[string]any, error) {
	out := make(map[string]any, len(qs))
	for id, q := range qs {
		m := map[string]any{"type": q.Type, "instructions": q.Instructions}
		switch q.Type {
		case "noul":
			if q.Criteria != nil {
				m["criteria"] = q.Criteria
			}
		case "choice":
			if len(q.Criteria) == 0 {
				return nil, fmt.Errorf("question %s: choice needs criteria options", id)
			}
			m["criteria"] = q.Criteria
		case "score":
			if len(q.Levels) < 2 || len(q.Levels) > 10 {
				return nil, fmt.Errorf("question %s: score needs 2-10 levels", id)
			}
			m["criteria"] = q.Levels
		default:
			return nil, fmt.Errorf("question %s: unknown type %q", id, q.Type)
		}
		out[id] = m
	}
	return out, nil
}

// Answer is one System One answer.
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        []string           `json:"legend,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// EvalResult is a /v1/systemone response.
type EvalResult struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   map[string]any    `json:"usage,omitempty"`
}

// Client calls System One.
type Client struct {
	BaseURL    string
	APIKey     string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// NewClient builds a client; empty model defaults to jev-latest.
func NewClient(baseURL, apiKey, model string) *Client {
	if model == "" {
		model = config.DefaultJevModel
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Timeout: 30 * time.Second,
	}
}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("systemone HTTP %d: %s", e.Status, e.Body)
}

// Evaluate sends one judgment request, with 429/529 backoff.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (*EvalResult, error) {
	if len(questions) == 0 {
		return nil, fmt.Errorf("questions must be non-empty")
	}
	qs, err := marshalQuestions(questions)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"model":     c.Model,
		"state":     state,
		"questions": qs,
	})
	if err != nil {
		return nil, err
	}
	url := c.BaseURL + "/systemone"
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: c.Timeout}
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("systemone request failed: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			lastErr = &httpError{Status: resp.StatusCode, Body: truncate(string(raw), maxErrorBody)}
			if !sleepCtx(ctx, time.Duration(attempt+1)*600*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &httpError{Status: resp.StatusCode, Body: truncate(string(raw), maxErrorBody)}
		}
		var out EvalResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("systemone response is not JSON: %w", err)
		}
		for id := range questions {
			if _, ok := out.Answers[id]; !ok {
				return nil, fmt.Errorf("systemone response missing answer %q", id)
			}
		}
		return &out, nil
	}
	return nil, lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
