package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fare-brain/config"
	"fare-brain/models"
)

// Hotel booking via Skyvern (P4 owns the checkout page + prompt tuning; signatures are the
// contract). StartHotelBooking returns immediately with a run id; WaitForBooking polls until
// done. The orchestrator calls these ONLY from the approval flow, after state.AssertCanBook.

var terminalStatuses = map[string]bool{
	"completed": true, "failed": true, "terminated": true, "timed_out": true, "canceled": true,
}

func BuildPrompt(hotel models.HotelOffer, guests int, leadName, leadEmail string) string {
	return fmt.Sprintf(`Book a room at %s for %d guests,
check-in %s, check-out %s.
Guest name: %s. Email: %s.
Pay with test card 4242 4242 4242 4242, expiry 12/34, CVC 123, postal code V5H 1A1.
The task is complete when a page shows a booking confirmation number.
If the total shown is more than %.0f %s, stop without paying.`,
		hotel.Name, guests, hotel.CheckIn, hotel.CheckOut, leadName, leadEmail,
		hotel.TotalPrice*1.1, hotel.Currency)
}

// StartHotelBooking kicks off the booking and returns (runID, liveURL). Does not wait.
func StartHotelBooking(ctx context.Context, cfg *config.Settings, hotel models.HotelOffer, guests int, leadName, leadEmail, title string) (string, string, error) {
	if cfg.MockBrowser {
		return "mock_run_" + strings.ToLower(randomCode(8, upperAlnum)), "", nil
	}
	// TODO(P4): verify this request/response shape against current Skyvern API docs before
	// flipping MOCK_BROWSER off — ported from the Python skyvern SDK's run_task() call, not yet
	// re-verified as raw HTTP.
	url := hotel.CheckoutURL
	if url == "" {
		url = cfg.HotelCheckoutURL
	}
	body, err := json.Marshal(map[string]any{
		"prompt": BuildPrompt(hotel, guests, leadName, leadEmail),
		"url":    url,
		"data_extraction_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirmation_number": map[string]string{"type": "string"},
				"hotel_name":          map[string]string{"type": "string"},
				"total_price":         map[string]string{"type": "string"},
				"check_in":            map[string]string{"type": "string"},
				"check_out":           map[string]string{"type": "string"},
			},
		},
		"max_steps": cfg.SkyvernMaxSteps,
		"title":     title,
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.skyvern.com/v1/runs/tasks", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", cfg.SkyvernAPIKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("skyvern run_task: status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		RunID  string `json:"run_id"`
		AppURL string `json:"app_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", err
	}
	return out.RunID, out.AppURL, nil
}

func WaitForBooking(ctx context.Context, cfg *config.Settings, runID string, pollInterval, timeout time.Duration) (*models.HotelBookingResult, error) {
	if cfg.MockBrowser {
		time.Sleep(time.Duration(cfg.MockBookingDelaySeconds * float64(time.Second)))
		code := randomCode(8, upperAlnum)
		return &models.HotelBookingResult{Status: "completed", ConfirmationNumber: "HV-" + code}, nil
	}

	// TODO(P4): verify polling response shape against current Skyvern API docs.
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 15 * time.Second}
	var run struct {
		Status         string   `json:"status"`
		Output         any      `json:"output"`
		RecordingURL   string   `json:"recording_url"`
		ScreenshotURLs []string `json:"screenshot_urls"`
		FailureReason  string   `json:"failure_reason"`
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.skyvern.com/v1/runs/"+runID, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", cfg.SkyvernAPIKey)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		if terminalStatuses[run.Status] {
			break
		}
		if time.Now().After(deadline) {
			return &models.HotelBookingResult{Status: "timed_out", FailureReason: "Gave up waiting for Skyvern"}, nil
		}
		time.Sleep(pollInterval)
	}

	output, _ := run.Output.(map[string]any)
	if output == nil {
		if list, ok := run.Output.([]any); ok && len(list) > 0 {
			output, _ = list[0].(map[string]any)
		}
	}
	var price *float64
	if output != nil {
		if raw, ok := output["total_price"]; ok && raw != nil {
			s := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(fmt.Sprint(raw), "$", ""), ",", ""))
			if len(s) > 0 {
				if f, err := strconv.ParseFloat(s[0], 64); err == nil {
					price = &f
				}
			}
		}
	}
	var confirmation string
	if output != nil {
		if s, ok := output["confirmation_number"].(string); ok {
			confirmation = s
		}
	}
	return &models.HotelBookingResult{
		Status: run.Status, ConfirmationNumber: confirmation, TotalPrice: price,
		RecordingURL: run.RecordingURL, ScreenshotURLs: run.ScreenshotURLs, FailureReason: run.FailureReason,
	}, nil
}
