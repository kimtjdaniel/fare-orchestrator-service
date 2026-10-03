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

const geminiAPIBase = "https://generativelanguage.googleapis.com/v1beta/models"

// GeminiLLM talks to the Gemini API's generateContent endpoint over raw net/http, mirroring
// ClaudeLLM's shape (same disk cache on exact request, same LLM interface) so main.go can pick
// either provider by which API key is set. Delete CacheDir to force fresh calls.
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
	return &GeminiLLM{APIKey: apiKey, Model: model, CacheDir: cacheDir, HTTPClient: &http.Client{Timeout: 60 * time.Second}}
}

// ---------- Gemini wire format ----------

type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFuncResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"` // "user" or "model"
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
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
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// textMessageToContent converts one of our provider-agnostic llm.Message turns (always a plain
// string in practice — see orchestrator/brain.go's call sites) into Gemini's content shape.
// "assistant" maps to Gemini's "model" role; anything else passes through as "user".
func textMessageToContent(m Message) geminiContent {
	role := "user"
	if m.Role == "assistant" || m.Role == "model" {
		role = "model"
	}
	text, _ := m.Content.(string)
	if text == "" {
		if raw, err := json.Marshal(m.Content); err == nil {
			text = string(raw)
		}
	}
	return geminiContent{Role: role, Parts: []geminiPart{{Text: text}}}
}

// create posts a generateContent request, caching on the exact request body.
func (g *GeminiLLM) create(ctx context.Context, body map[string]any) (*geminiResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var cachePath string
	if g.CacheDir != "" {
		sum := sha256.Sum256(payload)
		key := hex.EncodeToString(sum[:])[:32]
		cachePath = filepath.Join(g.CacheDir, "gemini-"+key+".json")
		if cached, err := os.ReadFile(cachePath); err == nil {
			slog.Info("llm cache hit", "key", key)
			var resp geminiResponse
			if err := json.Unmarshal(cached, &resp); err != nil {
				return nil, err
			}
			return &resp, nil
		}
	}

	url := fmt.Sprintf("%s/%s:generateContent?key=%s", geminiAPIBase, g.Model, g.APIKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")

	t0 := time.Now()
	httpResp, err := g.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	var resp geminiResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("gemini api %d: %.300s", httpResp.StatusCode, raw)
	}
	if httpResp.StatusCode >= 400 || resp.Error != nil {
		msg := string(raw)
		if resp.Error != nil {
			msg = resp.Error.Message
		}
		return nil, fmt.Errorf("gemini api %d: %s", httpResp.StatusCode, msg)
	}
	slog.Info("llm call", "model", g.Model, "elapsed", time.Since(t0),
		"in", resp.UsageMetadata.PromptTokenCount, "out", resp.UsageMetadata.CandidatesTokenCount)

	if cachePath != "" {
		_ = os.WriteFile(cachePath, raw, 0o644)
	}
	return &resp, nil
}

func (g *GeminiLLM) Structured(ctx context.Context, system string, messages []Message, schema Schema) (map[string]any, error) {
	contents := make([]geminiContent, len(messages))
	for i, m := range messages {
		contents[i] = textMessageToContent(m)
	}

	resp, err := g.create(ctx, map[string]any{
		"system_instruction": map[string]any{"parts": []map[string]string{{"text": system}}},
		"contents":           contents,
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   schema.Schema,
		},
	})
	if err != nil {
		return nil, err
	}
	text := geminiTextOf(resp)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("%s: Gemini returned non-JSON: %.200s", schema.Name, text)
	}
	return out, nil
}

func (g *GeminiLLM) Agent(ctx context.Context, system string, messages []Message, tools []Tool, handlers map[string]Handler) (string, error) {
	contents := make([]geminiContent, len(messages))
	for i, m := range messages {
		contents[i] = textMessageToContent(m)
	}

	decls := make([]geminiFunctionDecl, len(tools))
	for i, t := range tools {
		decls[i] = geminiFunctionDecl{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}
	}
	geminiTools := []geminiTool{{FunctionDeclarations: decls}}

	const maxTurns = 6
	for i := 0; i < maxTurns; i++ {
		resp, err := g.create(ctx, map[string]any{
			"system_instruction": map[string]any{"parts": []map[string]string{{"text": system}}},
			"contents":           contents,
			"tools":              geminiTools,
		})
		if err != nil {
			return "", err
		}
		if len(resp.Candidates) == 0 {
			return "Sorry, I didn't get a response. Can you rephrase?", nil
		}
		modelContent := resp.Candidates[0].Content

		var calls []geminiPart
		for _, p := range modelContent.Parts {
			if p.FunctionCall != nil {
				calls = append(calls, p)
			}
		}
		if len(calls) == 0 {
			return geminiTextOf(resp), nil
		}

		contents = append(contents, geminiContent{Role: "model", Parts: modelContent.Parts})

		var responseParts []geminiPart
		for _, p := range calls {
			call := p.FunctionCall
			handler, ok := handlers[call.Name]
			if !ok {
				responseParts = append(responseParts, geminiPart{
					FunctionResponse: &geminiFuncResponse{Name: call.Name, Response: map[string]any{"error": "no handler for " + call.Name}},
				})
				continue
			}
			output, err := handler(ctx, call.Args)
			if err != nil {
				slog.Error("tool failed", "tool", call.Name, "err", err)
				responseParts = append(responseParts, geminiPart{
					FunctionResponse: &geminiFuncResponse{Name: call.Name, Response: map[string]any{"error": err.Error()}},
				})
				continue
			}
			outJSON, err := json.Marshal(output)
			if err != nil {
				return "", err
			}
			var outMap map[string]any
			if err := json.Unmarshal(outJSON, &outMap); err != nil {
				// Gemini's functionResponse.response must be an object; wrap non-object results.
				outMap = map[string]any{"result": output}
			}
			responseParts = append(responseParts, geminiPart{
				FunctionResponse: &geminiFuncResponse{Name: call.Name, Response: outMap},
			})
		}
		contents = append(contents, geminiContent{Role: "user", Parts: responseParts})
	}
	return "Sorry, I got stuck working that out. Can you rephrase?", nil
}

func geminiTextOf(resp *geminiResponse) string {
	if len(resp.Candidates) == 0 {
		return ""
	}
	var out string
	for _, p := range resp.Candidates[0].Content.Parts {
		out += p.Text
	}
	return out
}
