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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const geminiAPIBase = "https://generativelanguage.googleapis.com/v1beta/models"

// GeminiLLM talks to the Gemini generateContent REST API over raw net/http. Disk-caches on the
// exact request: same transcript in -> same response out, no API call. Delete CacheDir to force
// fresh calls.
type GeminiLLM struct {
	APIKey     string
	Model      string
	CacheDir   string
	HTTPClient *http.Client
}

func NewGeminiLLM(apiKey, model, cacheDir string) *GeminiLLM {
	if cacheDir != "" {
		_ = os.MkdirAll(cacheDir, 0o755)
	}
	if model == "" {
		model = "gemini-3.1-flash-lite"
	}
	return &GeminiLLM{APIKey: apiKey, Model: model, CacheDir: cacheDir, HTTPClient: &http.Client{Timeout: 40 * time.Second}}
}

func (g *GeminiLLM) modelsToTry() []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	add(g.Model)
	add("gemini-3.1-flash-lite")
	add("gemini-3.5-flash-lite")
	return out
}

type geminiPart struct {
	Text         string          `json:"text,omitempty"`
	FunctionCall *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResp *geminiFuncResp `json:"functionResponse,omitempty"`
}

type geminiFuncCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

type geminiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// generate posts a generateContent request, caching on the exact request body.
func (g *GeminiLLM) generate(ctx context.Context, kind string, body map[string]any) (*geminiResponse, error) {
	applyFastGen(body)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var cachePath string
	if g.CacheDir != "" {
		sum := sha256.Sum256(payload)
		key := hex.EncodeToString(sum[:])[:32]
		cachePath = filepath.Join(g.CacheDir, key+".json")
		if cached, err := os.ReadFile(cachePath); err == nil {
			slog.Info("llm cache hit", "kind", kind, "key", key)
			var resp geminiResponse
			if err := json.Unmarshal(cached, &resp); err != nil {
				return nil, err
			}
			return &resp, nil
		}
	}

	t0 := time.Now()
	preview := lastContentPreview(body)
	slog.Info("prompting gemini", "kind", kind, "model", g.Model, "bytes", len(payload), "preview", preview)

	var lastErr error
	for _, model := range g.modelsToTry() {
		for attempt := 1; attempt <= 2; attempt++ {
			endpoint := fmt.Sprintf("%s/%s:generateContent?key=%s", geminiAPIBase, model, url.QueryEscape(g.APIKey))
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			httpReq.Header.Set("content-type", "application/json")

			httpResp, err := g.HTTPClient.Do(httpReq)
			if err != nil {
				lastErr = err
				break
			}
			raw, err := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			if err != nil {
				return nil, err
			}
			if httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode == http.StatusServiceUnavailable {
				var apiErr geminiError
				_ = json.Unmarshal(raw, &apiErr)
				lastErr = fmt.Errorf("gemini api %d (%s): %s", httpResp.StatusCode, model, apiErr.Error.Message)
				slog.Warn("gemini busy, retrying", "kind", kind, "model", model, "attempt", attempt, "status", httpResp.StatusCode)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(400 * time.Millisecond):
				}
				continue
			}
			if httpResp.StatusCode >= 400 {
				var apiErr geminiError
				_ = json.Unmarshal(raw, &apiErr)
				lastErr = fmt.Errorf("gemini api %d (%s): %s", httpResp.StatusCode, model, apiErr.Error.Message)
				slog.Warn("gemini rejected model, trying next", "model", model, "status", httpResp.StatusCode, "err", apiErr.Error.Message)
				break
			}

			var resp geminiResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				return nil, err
			}
			slog.Info("llm call", "kind", kind, "model", model, "elapsed", time.Since(t0),
				"in", resp.UsageMetadata.PromptTokenCount, "out", resp.UsageMetadata.CandidatesTokenCount)
			if cachePath != "" {
				_ = os.WriteFile(cachePath, raw, 0o644)
			}
			return &resp, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("gemini api: all models failed")
	}
	return nil, lastErr
}

func (g *GeminiLLM) Structured(ctx context.Context, system string, messages []Message, schema Schema) (map[string]any, error) {
	resp, err := g.generate(ctx, "structured:"+schema.Name, map[string]any{
		"system_instruction": geminiContent{Parts: []geminiPart{{Text: system}}},
		"contents":           toGeminiContents(messages),
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   toGeminiSchema(schema.Schema),
			"maxOutputTokens":  8192,
		},
	})
	if err != nil {
		return nil, err
	}
	text := textOf(resp)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		reason := ""
		if len(resp.Candidates) > 0 {
			reason = resp.Candidates[0].FinishReason
		}
		return nil, fmt.Errorf("%s: Gemini returned non-JSON (finishReason=%s): %.200s", schema.Name, reason, text)
	}
	return out, nil
}

func (g *GeminiLLM) Agent(ctx context.Context, system string, messages []Message, tools []Tool, handlers map[string]Handler) (string, error) {
	contents := toGeminiContents(messages)

	var geminiTools []geminiTool
	if len(tools) > 0 {
		decls := make([]geminiFunctionDecl, len(tools))
		for i, t := range tools {
			decls[i] = geminiFunctionDecl{Name: t.Name, Description: t.Description, Parameters: toGeminiSchema(t.InputSchema)}
		}
		geminiTools = []geminiTool{{FunctionDeclarations: decls}}
	}

	const maxTurns = 6
	for i := 0; i < maxTurns; i++ {
		body := map[string]any{
			"system_instruction": geminiContent{Parts: []geminiPart{{Text: system}}},
			"contents":           contents,
			"generationConfig":   map[string]any{"maxOutputTokens": 800},
		}
		if len(geminiTools) > 0 {
			body["tools"] = geminiTools
		}
		resp, err := g.generate(ctx, "agent", body)
		if err != nil {
			return "", err
		}
		if len(resp.Candidates) == 0 {
			return "Sorry, I got stuck working that out. Can you rephrase?", nil
		}
		candidate := resp.Candidates[0].Content

		var calls []geminiFuncCall
		for _, p := range candidate.Parts {
			if p.FunctionCall != nil {
				calls = append(calls, *p.FunctionCall)
			}
		}
		if len(calls) == 0 {
			return textOfParts(candidate.Parts), nil
		}

		contents = append(contents, geminiContent{Role: "model", Parts: candidate.Parts})

		var resultParts []geminiPart
		for _, call := range calls {
			handler, ok := handlers[call.Name]
			if !ok {
				resultParts = append(resultParts, geminiPart{FunctionResp: &geminiFuncResp{
					Name:     call.Name,
					Response: map[string]any{"error": fmt.Sprintf("no handler for tool %s", call.Name)},
				}})
				continue
			}
			output, err := handler(ctx, call.Args)
			if err != nil {
				slog.Error("tool failed", "tool", call.Name, "err", err)
				resultParts = append(resultParts, geminiPart{FunctionResp: &geminiFuncResp{
					Name:     call.Name,
					Response: map[string]any{"error": err.Error()},
				}})
				continue
			}
			outJSON, err := json.Marshal(output)
			if err != nil {
				return "", err
			}
			var outMap map[string]any
			if err := json.Unmarshal(outJSON, &outMap); err != nil {
				outMap = map[string]any{"result": output}
			}
			resultParts = append(resultParts, geminiPart{FunctionResp: &geminiFuncResp{Name: call.Name, Response: outMap}})
		}
		contents = append(contents, geminiContent{Role: "user", Parts: resultParts})
	}
	return "Sorry, I got stuck working that out. Can you rephrase?", nil
}

func toGeminiContents(messages []Message) []geminiContent {
	out := make([]geminiContent, 0, len(messages))
	for _, m := range messages {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		text, _ := m.Content.(string)
		out = append(out, geminiContent{Role: role, Parts: []geminiPart{{Text: text}}})
	}
	return out
}

func textOf(resp *geminiResponse) string {
	if len(resp.Candidates) == 0 {
		return ""
	}
	return textOfParts(resp.Candidates[0].Content.Parts)
}

func textOfParts(parts []geminiPart) string {
	var out string
	for _, p := range parts {
		out += p.Text
	}
	return out
}

// toGeminiSchema converts the plain JSON-Schema maps written in prompts.go/tools.go (lowercase
// type names, "additionalProperties": false) into Gemini's OpenAPI-subset Schema shape (uppercase
// type enum, no additionalProperties support).
func toGeminiSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		switch k {
		case "additionalProperties":
			continue
		case "type":
			if s, ok := v.(string); ok {
				out[k] = strings.ToUpper(s)
			} else {
				out[k] = v
			}
		case "properties":
			if props, ok := v.(map[string]any); ok {
				converted := make(map[string]any, len(props))
				for name, p := range props {
					if pm, ok := p.(map[string]any); ok {
						converted[name] = toGeminiSchema(pm)
					} else {
						converted[name] = p
					}
				}
				out[k] = converted
			} else {
				out[k] = v
			}
		case "items":
			if im, ok := v.(map[string]any); ok {
				out[k] = toGeminiSchema(im)
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

func applyFastGen(body map[string]any) {
	cfg, _ := body["generationConfig"].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	if _, ok := cfg["maxOutputTokens"]; !ok {
		cfg["maxOutputTokens"] = 1536
	}
	cfg["thinkingConfig"] = map[string]any{"thinkingBudget": 0}
	body["generationConfig"] = cfg
}

func lastContentPreview(body map[string]any) string {
	contents, _ := body["contents"].([]geminiContent)
	for i := len(contents) - 1; i >= 0; i-- {
		for j := len(contents[i].Parts) - 1; j >= 0; j-- {
			t := strings.TrimSpace(contents[i].Parts[j].Text)
			if t == "" {
				continue
			}
			t = strings.ReplaceAll(t, "\n", " ")
			if len(t) > 140 {
				return t[:140] + "…"
			}
			return t
		}
	}
	return ""
}
