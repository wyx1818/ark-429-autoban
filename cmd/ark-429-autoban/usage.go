package main

import (
	"encoding/json"
	"fmt"
	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
	"log/slog"
	"strings"
	"time"
)

// handleUsage observes a completed request. On an ARK 429 it records the ban.
func (p *plugin) handleUsage(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return okEnvelope(map[string]any{})
	}
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		slog.Warn("ark-429-autoban: failed to decode usage record", "error", errUnmarshal)
		return okEnvelope(map[string]any{})
	}

	// Only openai-compatibility credentials with the ARK base URL are in scope.
	authID := strings.TrimSpace(record.AuthID)
	if !isARKCredential(authID) {
		return okEnvelope(map[string]any{})
	}
	if !p.isARKAuth(authID) {
		return okEnvelope(map[string]any{})
	}

	// Only act on 429 failures.
	if !record.Failed || record.Failure.StatusCode != statusTooManyRequests {
		return okEnvelope(map[string]any{})
	}

	if authID == "" {
		slog.Warn("ark-429-autoban: 429 received but AuthID is empty, cannot ban")
		return okEnvelope(map[string]any{})
	}

	// Classify the 429 and build a ban entry.
	entry, parsedOK := classifyAndBuildBan(record.Failure.Body)
	errorCode := extractErrorCode(record.Failure.Body)

	if !parsedOK {
		// Body didn't have a parseable reset time. Use per-code fallback duration.
		p.mu.RLock()
		genericFallback := p.fallbackBan
		p.mu.RUnlock()

		fallbackDuration := genericFallback
		if d := errorCodeFallback(errorCode); d > 0 {
			fallbackDuration = d
		}

		minutes := int(fallbackDuration.Minutes())
		slog.Warn("ark-429-autoban: could not parse reset time from 429 body, falling back to ban",
			"auth_id", authID, "key_hint", p.keyHintFor(authID),
			"error_code", errorCode,
			"fallback_minutes", minutes,
			"body_preview", truncateBody(record.Failure.Body, 300))

		now := p.now()
		windowLabel := fmt.Sprintf("%dm (fallback)", minutes)
		entry = banEntry{
			ResetAt:   now.Add(fallbackDuration),
			Window:    windowLabel,
			BannedAt:  now,
			KeyHint:   p.keyHintFor(authID),
			ErrorCode: errorCode,
		}
	} else {
		entry.BannedAt = p.now()
		entry.KeyHint = p.keyHintFor(authID)
		entry.ErrorCode = errorCode
	}

	// Check existing ban: only extend, never shorten.
	if existing, ok := p.bans.lookup(authID); ok {
		if p.now().Before(existing.ResetAt) {
			if !entry.ResetAt.After(existing.ResetAt) {
				// New reset time is not later - keep existing ban.
				return okEnvelope(map[string]any{})
			}
			// New reset time is later - extend the ban.
			// Preserve original BannedAt and KeyHint if already set.
			entry.BannedAt = existing.BannedAt
			if entry.KeyHint == "" {
				entry.KeyHint = existing.KeyHint
			}
			slog.Info("ark-429-autoban: extending ban for credential",
				"auth_id", authID,
				"old_reset_at", existing.ResetAt.Format(time.RFC3339),
				"new_reset_at", entry.ResetAt.Format(time.RFC3339),
				"window", entry.Window)
		}
	}

	p.bans.set(authID, entry)
	p.markDirty()
	slog.Info("ark-429-autoban: banned credential after 429",
		"auth_id", authID,
		"key_hint", entry.KeyHint,
		"error_code", entry.ErrorCode,
		"window", entry.Window,
		"reset_at", entry.ResetAt.Format(time.RFC3339))
	return okEnvelope(map[string]any{})
}

// classifyAndBuildBan parses the ARK 429 response body to extract the quota
// reset time. ARK returns a JSON error like:
//
//	{
//	  "error": {
//	    "code": "AccountQuotaExceeded",
//	    "message": "You have exceeded the monthly usage quota. It will reset at 2026-08-03 23:59:59 +0800 CST. ...",
//	    "type": "TooManyRequests"
//	  }
//	}
//
// The reset time is embedded in the message string, not a structured field.
// We extract it with a regex and parse it as a time.
func classifyAndBuildBan(body string) (banEntry, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return banEntry{}, false
	}

	// Try to parse as JSON and extract the error message.
	var errObj struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &errObj); errUnmarshal != nil {
		// Not JSON; try regex on the raw body.
		return parseResetTimeFromString(body)
	}

	// Check if this is a quota exceeded error.
	code := strings.TrimSpace(errObj.Error.Code)
	if code == "" {
		// Some providers put the code in "type".
		code = strings.TrimSpace(errObj.Error.Type)
	}

	// Accept any 429 with a parseable reset time, but log the code for diagnostics.
	window := code
	if window == "" {
		window = "quota"
	}
	// Extract quota period from message, e.g. "exceeded the 5-hour usage quota"
	// or "exceeded the monthly usage quota".
	if period := extractQuotaPeriod(errObj.Error.Message); period != "" {
		window = period
	}

	msg := errObj.Error.Message
	if msg == "" {
		return banEntry{}, false
	}

	entry, ok := parseResetTimeFromString(msg)
	if !ok {
		return banEntry{}, false
	}
	entry.Window = window
	return entry, true
}

// parseResetTimeFromString extracts the "It will reset at <timestamp>" from
// the given text and parses it into a time.Time.
// The timestamp format is like "2026-08-03 23:59:59 +0800 CST".
func parseResetTimeFromString(text string) (banEntry, bool) {
	matches := resetTimeRe.FindStringSubmatch(text)
	if len(matches) < 2 {
		return banEntry{}, false
	}

	// ARK uses a Go-style time format: "2006-01-02 15:04:05 -0700 MST"
	// This is the Go reference time layout.
	layout := "2006-01-02 15:04:05 -0700 MST"
	t, err := time.Parse(layout, matches[1])
	if err != nil {
		// Try without the timezone name (CST part), which can be ambiguous.
		layout2 := "2006-01-02 15:04:05 -0700"
		// Strip the trailing timezone abbreviation.
		parts := strings.Fields(matches[1])
		if len(parts) >= 4 {
			stripped := strings.Join(parts[:3], " ")
			t, err = time.Parse(layout2, stripped)
		}
		if err != nil {
			slog.Warn("ark-429-autoban: failed to parse reset time",
				"raw", matches[1], "error", err)
			return banEntry{}, false
		}
	}

	return banEntry{ResetAt: t}, true
}
