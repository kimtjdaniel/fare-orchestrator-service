// Command smoke runs one-command checks that each real service is wired up. Run after filling
// in .env.
//
//	go run ./scripts/smoke db        # connect, ensure indexes, list collections    (free)
//	go run ./scripts/smoke gemini    # one tiny structured-output call              (~$0.001)
//	go run ./scripts/smoke schemas   # send all 3 brain schemas once, real transcript (~$0.02)
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

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

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

func smokeSchemas(cfg *config.Settings) {
	c := geminiClient(cfg)
	raw, err := os.ReadFile("testdata/demo_transcript.json")
	if err != nil {
		fail(err.Error())
	}
	var transcript struct {
		Messages []struct {
			SenderName string `json:"sender_name"`
			Text       string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &transcript); err != nil {
		fail(err.Error())
	}
	chat := ""
	for i, m := range transcript.Messages {
		if i > 0 {
			chat += "\n"
		}
		chat += fmt.Sprintf("%s: %s", m.SenderName, m.Text)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc).Format("2006-01-02")
	ctx := context.Background()

	prefs, err := c.Structured(ctx, prompts.ExtractSystem(today),
		[]llm.Message{{Role: "user", Content: fmt.Sprintf("Group chat:\n%s", chat)}}, toSchema(prompts.RecordPreferences))
	if err != nil {
		fail(err.Error())
	}
	var names []string
	if people, ok := prefs["participants"].([]any); ok {
		for _, p := range people {
			if m, ok := p.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					names = append(names, n)
				}
			}
		}
	}
	ok(fmt.Sprintf("record_preferences: %v", names))
	printJSON(prefs)

	participantsJSON, _ := json.Marshal(prefs["participants"])
	opts, err := c.Structured(ctx, prompts.ProposeSystem(cfg.BotName, today, ""),
		[]llm.Message{{Role: "user", Content: "Participants:\n" + string(participantsJSON)}}, toSchema(prompts.ProposeOptions))
	if err != nil {
		fail(err.Error())
	}
	var destinations []string
	if options, ok := opts["options"].([]any); ok {
		for _, o := range options {
			if m, ok := o.(map[string]any); ok {
				if d, ok := m["destination"].(string); ok {
					destinations = append(destinations, d)
				}
			}
		}
	}
	ok(fmt.Sprintf("propose_options: %v", destinations))
	printJSON(opts)

	intent, err := c.Structured(ctx, prompts.InterpretSystem("AWAITING_CHOICE", "1. A; 2. B"),
		[]llm.Message{{Role: "user", Content: "@Fare let's do the second one"}}, toSchema(prompts.InterpretReply))
	if err != nil {
		fail(err.Error())
	}
	ok(fmt.Sprintf("interpret_reply: %v", intent))
	fmt.Println("\nTip: paste the record_preferences output into llm/mock_data.go to make mocks realistic.")
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
		fmt.Println("usage: smoke <db|gemini|schemas|telegram>")
		os.Exit(1)
	}
	cfg := config.Load()
	switch os.Args[1] {
	case "db":
		smokeDB(cfg)
	case "gemini":
		smokeGemini(cfg)
	case "schemas":
		smokeSchemas(cfg)
	case "telegram":
		smokeTelegram(cfg)
	default:
		fmt.Println("usage: smoke <db|gemini|schemas|telegram>")
		os.Exit(1)
	}
}
