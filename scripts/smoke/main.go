// Command smoke runs one-command checks that each real service is wired up. Run after filling
// in .env.
//
//	go run ./scripts/smoke db        # connect, ensure indexes, list collections    (free)
//	go run ./scripts/smoke gemini    # one tiny structured-output call              (~$0.001)
//	go run ./scripts/smoke telegram  # getMe + getWebhookInfo                       (free)
//
// The Python version's `skyvern` subcommand is dropped — there's no official Go SDK, and the
// raw-HTTP real branch in tools/browser.go is marked TODO(P4) for verification instead.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"fare-brain/config"
	"fare-brain/llm"
	"fare-brain/prompts"
	"fare-brain/store"
)

func ok(msg string)   { fmt.Printf("✅ %s\n", msg) }
func fail(msg string) { fmt.Printf("❌ %s\n", msg); os.Exit(1) }

func toSchema(m map[string]any) llm.Schema {
	name, _ := m["name"].(string)
	schema, _ := m["schema"].(map[string]any)
	return llm.Schema{Name: name, Schema: schema}
}

func smokeDB(cfg *config.Settings) {
	if cfg.MongoURI == "" {
		fail("MONGODB_URI is empty")
	}
	ms := store.NewMongoStore(cfg.MongoURI, cfg.MongoDB)
	ctx := context.Background()
	if err := ms.Connect(ctx); err != nil {
		fail(fmt.Sprintf("connect: %v", err))
	}
	defer ms.Close(ctx)
	ok(fmt.Sprintf("connected, indexes ensured, db=%s", cfg.MongoDB))
}

func geminiClient(cfg *config.Settings) *llm.GeminiLLM {
	if cfg.GeminiAPIKey == "" {
		fail("GEMINI_API_KEY is empty")
	}
	return llm.NewGeminiLLM(cfg.GeminiAPIKey, cfg.GeminiModel, "")
}

func smokeGemini(cfg *config.Settings) {
	c := geminiClient(cfg)
	out, err := c.Structured(context.Background(), "Classify the message.",
		[]llm.Message{{Role: "user", Content: "✅ book it!"}}, toSchema(prompts.InterpretReply))
	if err != nil {
		fail(err.Error())
	}
	ok(fmt.Sprintf("%s answered: %v", cfg.GeminiModel, out))
}

func smokeTelegram(cfg *config.Settings) {
	if cfg.TelegramBotToken == "" {
		fail("TELEGRAM_BOT_TOKEN is empty")
	}
	base := "https://api.telegram.org/bot" + cfg.TelegramBotToken
	client := &http.Client{Timeout: 10 * time.Second}

	me, err := getJSON(client, base+"/getMe")
	if err != nil {
		fail(err.Error())
	}
	if okField, _ := me["ok"].(bool); !okField {
		fail(fmt.Sprintf("getMe failed: %v", me))
	}
	result, _ := me["result"].(map[string]any)
	username, _ := result["username"].(string)
	ok(fmt.Sprintf("bot @%s (set TELEGRAM_BOT_USERNAME=%s)", username, username))
	fmt.Printf("   can_read_all_group_messages: %v  <- must be true (BotFather /setprivacy -> Disable, then re-add bot to the group)\n",
		result["can_read_all_group_messages"])

	info, err := getJSON(client, base+"/getWebhookInfo")
	if err != nil {
		fail(err.Error())
	}
	infoResult, _ := info["result"].(map[string]any)
	webhookURL, _ := infoResult["url"].(string)
	if webhookURL == "" {
		webhookURL = "(not set)"
	}
	lastError, _ := infoResult["last_error_message"].(string)
	if lastError == "" {
		lastError = "none"
	}
	fmt.Printf("   webhook: %s  pending: %v  last_error: %s\n",
		webhookURL, infoResult["pending_update_count"], lastError)
}

func getJSON(client *http.Client, url string) (map[string]any, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: smoke <db|gemini|telegram>")
		os.Exit(1)
	}
	cfg := config.Load()
	switch os.Args[1] {
	case "db":
		smokeDB(cfg)
	case "gemini":
		smokeGemini(cfg)
	case "telegram":
		smokeTelegram(cfg)
	default:
		fmt.Println("usage: smoke <db|gemini|telegram>")
		os.Exit(1)
	}
}
