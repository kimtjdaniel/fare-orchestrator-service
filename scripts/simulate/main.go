// Command simulate plays a group chat yourself — no WhatsApp, Telegram, or server needed.
//
//	go run ./scripts/simulate                               # chat interactively from an empty group
//	go run ./scripts/simulate --url http://localhost:8000   # send to a running server instead
//
// Interactive input format:   Name: message      (mention the bot with @Fare)
// Uses your .env, so with MOCK_LLM=false it calls the real LLM (cached in LLM_CACHE_DIR).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"fare-brain/config"
	"fare-brain/llm"
	"fare-brain/messaging"
	"fare-brain/models"
	"fare-brain/orchestrator"
	"fare-brain/store"
)

func main() {
	url := flag.String("url", "", "POST to a running server's /webhook instead of in-process")
	flag.Parse()

	cfg := config.Load()
	groupID := fmt.Sprintf("sim-%d", time.Now().Unix())

	var deliver func(m models.IncomingMessage)
	ctx := context.Background()

	if *url != "" {
		client := &http.Client{Timeout: 10 * time.Second}
		deliver = func(m models.IncomingMessage) {
			body, _ := json.Marshal(m)
			resp, err := client.Post(*url+"/webhook", "application/json", bytes.NewReader(body))
			if err != nil {
				fmt.Fprintln(os.Stderr, "post /webhook:", err)
				return
			}
			resp.Body.Close()
		}
	} else {
		cfg.MessagingBackend = "console"
		st := store.NewMemoryStore()
		if err := st.Connect(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "store connect:", err)
			os.Exit(1)
		}
		var llmClient llm.LLM
		if cfg.MockLLM {
			llmClient = &llm.MockLLM{}
		} else {
			if cfg.GeminiAPIKey == "" {
				fmt.Fprintln(os.Stderr, "MOCK_LLM=false but GEMINI_API_KEY is empty")
				os.Exit(1)
			}
			llmClient = llm.NewGeminiLLM(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMCacheDir)
		}
		messenger := &messaging.ConsoleMessenger{}
		brain := orchestrator.NewBrain(cfg, st, llmClient, messenger)
		deliver = func(m models.IncomingMessage) {
			brain.Handle(ctx, m)
		}
	}

	llmLabel := cfg.GeminiModel
	if cfg.MockLLM {
		llmLabel = "mock"
	}
	fmt.Printf("group: %s | llm=%s\n", groupID, llmLabel)

	fmt.Println("\nType messages as  Name: message  (Ctrl+C to quit).")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		name, text, ok := strings.Cut(line, ":")
		if !ok {
			fmt.Println("  format is  Name: message")
			continue
		}
		name, text = strings.TrimSpace(name), strings.TrimSpace(text)
		tagged := strings.Contains(strings.ToLower(text), "@"+strings.ToLower(cfg.BotName))
		deliver(models.IncomingMessage{
			GroupID: groupID, GroupName: "Simulator", SenderID: "u_" + strings.ToLower(name),
			SenderName: name, Text: text, Tagged: tagged, Timestamp: time.Now().Unix(),
		})
	}
}
