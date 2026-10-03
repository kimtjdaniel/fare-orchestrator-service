// Package config holds all settings in one place, read from environment / .env. Defaults = fully
// mocked, no keys needed.
package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Settings struct {
	MockLLM                 bool
	MockTravel              bool
	MockBrowser             bool
	MockBookingDelaySeconds float64

	GeminiAPIKey string
	GeminiModel  string
	LLMCacheDir  string

	MongoURI string
	MongoDB  string

	MessagingBackend      string
	RobotURL              string
	RobotToken            string
	TelegramBotToken      string
	TelegramBotUsername   string
	TelegramWebhookSecret string
	BotName               string
	Timezone              string

	SkyvernAPIKey    string
	HotelCheckoutURL string
	SkyvernMaxSteps  int

	DashboardURL string
}

func boolEnv(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(getenv(name, "")))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func getenv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func intEnv(name string, def int) int {
	v := getenv(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func floatEnv(name string, def float64) float64 {
	v := getenv(name, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// Load reads .env (if present) and returns the resolved Settings.
func Load() *Settings {
	// Overload so a local .env wins over an empty SERVICE_TOKEN inherited from the shell.
	_ = godotenv.Overload()
	return &Settings{
		MockLLM:                 boolEnv("MOCK_LLM", true),
		MockTravel:              boolEnv("MOCK_TRAVEL", true),
		MockBrowser:             boolEnv("MOCK_BROWSER", true),
		MockBookingDelaySeconds: floatEnv("MOCK_BOOKING_DELAY", 3),

		GeminiAPIKey: firstNonEmpty(getenv("GEMINI_API_KEY", ""), getenv("GOOGLE_API_KEY", "")),
		GeminiModel:  getenv("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		LLMCacheDir:  getenv("LLM_CACHE_DIR", ""),

		MongoURI: getenv("MONGODB_URI", ""),
		MongoDB:  getenv("MONGODB_DB", "fare"),

		MessagingBackend:      getenv("MESSAGING_BACKEND", "console"),
		RobotURL:              getenv("ROBOT_URL", "http://localhost:3000"),
		RobotToken:            firstNonEmpty(getenv("ROBOT_TOKEN", ""), getenv("SERVICE_TOKEN", "")),
		TelegramBotToken:      getenv("TELEGRAM_BOT_TOKEN", ""),
		TelegramBotUsername:   getenv("TELEGRAM_BOT_USERNAME", ""),
		TelegramWebhookSecret: getenv("TELEGRAM_WEBHOOK_SECRET", ""),
		BotName:               getenv("BOT_NAME", "Fare"),
		Timezone:              getenv("TIMEZONE", "America/Vancouver"),

		SkyvernAPIKey:    getenv("SKYVERN_API_KEY", ""),
		HotelCheckoutURL: getenv("HOTEL_CHECKOUT_URL", "https://example-fake-hotel.vercel.app"),
		SkyvernMaxSteps:  intEnv("SKYVERN_MAX_STEPS", 25),

		DashboardURL: getenv("DASHBOARD_URL", "http://localhost:3001"),
	}
}
