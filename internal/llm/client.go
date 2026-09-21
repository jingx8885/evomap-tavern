// Package llm is a minimal chat-completions client on the same new-api
// gateway, used by the long-term planner. Jev gates when to call it.
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jingx8885/lov-evo/internal/config"
)

// Client calls /chat/completions.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

// NewClient builds an LLM client; empty model defaults to the planner model.
func NewClient(baseURL, apiKey, model string) *Client {
	if model == "" {
		model = config.DefaultPlannerModel
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Timeout: 30 * time.Second,
	}
}

// ChatComplete sends system+user and returns the text of the first choice.
func (c *Client) ChatComplete(ctx context.Context, system, user string) (string, error) {
	return c.roundTrip(ctx, c.Model, []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": user},
	}, 0.6)
}

// ChatVision sends a JPEG as a data-URL image_url part. Same gateway as ChatComplete.
func (c *Client) ChatVision(ctx context.Context, system, user string, jpeg []byte) (string, error) {
	if len(jpeg) == 0 {
		return c.ChatComplete(ctx, system, user)
	}
	userContent := []map[string]any{
		{"type": "text", "text": user},
		{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg),
		}},
	}
	timeout := c.Timeout
	if timeout < 45*time.Second {
		timeout = 45 * time.Second
	}
	return c.roundTripTimeout(ctx, c.Model, []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": userContent},
	}, 0.2, timeout)
}

func (c *Client) roundTrip(ctx context.Context, model string, messages []any, temp float64) (string, error) {
	return c.roundTripTimeout(ctx, model, messages, temp, c.Timeout)
}

func (c *Client) roundTripTimeout(ctx context.Context, model string, messages []any, temp float64, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	body, err := json.Marshal(map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": temp,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm HTTP %d: %s", resp.StatusCode,
			first(string(raw), 400))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm response is not JSON: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("llm returned no content")
	}
	return out.Choices[0].Message.Content, nil
}

func first(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
