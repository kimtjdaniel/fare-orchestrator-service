package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	apiURL     = "https://api.anthropic.com/v1/messages"
	apiVersion = "2023-06-01"
)

// ClaudeLLM talks to the real Messages API over raw net/http (no SDK, so output_config is never
// blocked by whatever the SDK's typed request struct does or doesn't expose). Disk-caches on the
// exact request: same transcript in -> same response out, no API call. Delete CacheDir to force
// fresh calls.
type ClaudeLLM struct {
	APIKey     string
	Model      string
	CacheDir   string
	HTTPClient *http.Client
}

func NewClaudeLLM(apiKey, model, cacheDir string) *ClaudeLLM {
	if cacheDir != "" {
		_ = os.MkdirAll(cacheDir, 0o755)
	}
	return &ClaudeLLM{APIKey: apiKey, Model: model, CacheDir: cacheDir, HTTPClient: &http.Client{Timeout: 60 * time.Second}}
}

type contentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

type apiResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// create posts a Messages API request, caching on the exact request body.
func (c *ClaudeLLM) create(ctx context.Context, body map[string]any) (*apiResponse, error) {
	req := map[string]any{"model": c.Model, "max_tokens": 2048}
	for k, v := range body {
		req[k] = v
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var cachePath string
	if c.CacheDir != "" {
		sum := sha256.Sum256(payload)
		key := hex.EncodeToString(sum[:])[:32]
		cachePath = filepath.Join(c.CacheDir, key+".json")
		if cached, err := os.ReadFile(cachePath); err == nil {
			slog.Info("llm cache hit", "key", key)
			var resp apiResponse
			if err := json.Unmarshal(cached, &resp); err != nil {
				return nil, err
			}
			return &resp, nil
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	t0 := time.Now()
	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode >= 400 {
		var apiErr apiError
		_ = json.Unmarshal(raw, &apiErr)
		return nil, fmt.Errorf("claude api %d: %s", httpResp.StatusCode, apiErr.Error.Message)
	}

	var resp apiResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	slog.Info("llm call", "model", c.Model, "elapsed", time.Since(t0),
		"in", resp.Usage.InputTokens, "out", resp.Usage.OutputTokens)

	if cachePath != "" {
		_ = os.WriteFile(cachePath, raw, 0o644)
	}
	return &resp, nil
}

func (c *ClaudeLLM) Structured(ctx context.Context, system string, messages []Message, schema Schema) (map[string]any, error) {
	resp, err := c.create(ctx, map[string]any{
		"system":   system,
		"messages": messages,
		"output_config": map[string]any{
			"format": map[string]any{"type": "json_schema", "schema": schema.Schema},
		},
	})
	if err != nil {
		return nil, err
	}
	text := textOf(resp)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("%s: Claude returned non-JSON (stop_reason=%s): %.200s", schema.Name, resp.StopReason, text)
	}
	return out, nil
}

func (c *ClaudeLLM) Agent(ctx context.Context, system string, messages []Message, tools []Tool, handlers map[string]Handler) (string, error) {
	msgs := append([]Message{}, messages...)
	const maxTurns = 6
	for i := 0; i < maxTurns; i++ {
		resp, err := c.create(ctx, map[string]any{
			"system":   system,
			"messages": msgs,
			"tools":    tools,
		})
		if err != nil {
			return "", err
		}
		if resp.StopReason != "tool_use" {
			return textOf(resp), nil
		}

		msgs = append(msgs, Message{Role: "assistant", Content: resp.Content})

		var results []contentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			handler, ok := handlers[block.Name]
			if !ok {
				results = append(results, contentBlock{Type: "tool_result", ToolUseID: block.ID,
					Content: fmt.Sprintf("Error: no handler for tool %s", block.Name), IsError: true})
				continue
			}
			output, err := handler(ctx, block.Input)
			if err != nil {
				slog.Error("tool failed", "tool", block.Name, "err", err)
				results = append(results, contentBlock{Type: "tool_result", ToolUseID: block.ID,
					Content: fmt.Sprintf("Error: %s", err), IsError: true})
				continue
			}
			outJSON, err := json.Marshal(output)
			if err != nil {
				return "", err
			}
			results = append(results, contentBlock{Type: "tool_result", ToolUseID: block.ID, Content: string(outJSON)})
		}
		msgs = append(msgs, Message{Role: "user", Content: results})
	}
	return "Sorry, I got stuck working that out. Can you rephrase?", nil
}

func textOf(resp *apiResponse) string {
	var out string
	for _, b := range resp.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}
