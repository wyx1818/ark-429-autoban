package main

import (
	"encoding/json"
	"fmt"
	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
	"log/slog"
)

// handleSchedulerPick filters out credentials that are still banned, then
// delegates the actual selection to the built-in round-robin scheduler.
func (p *plugin) handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	now := p.now()
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	bannedCount := 0
	for _, candidate := range req.Candidates {
		// The config loader has already validated the provider base URL and
		// registered every ARK credential by its CPA auth ID.
		if !isARKCredential(candidate.ID) || !p.isARKAuth(candidate.ID) {
			available = append(available, candidate)
			continue
		}
		// Cache key abbreviation for display in ban logs and management API.
		// Do this BEFORE checking ban status, so banned keys still get backfilled.
		// CPA doesn't expose api_key in scheduler candidates, so build a hint
		// from compat_name + config_index instead.
		hint := p.buildHintFromAttrs(candidate.ID, candidate.Attributes)
		if hint != "" {
			p.mu.Lock()
			p.keyHints[candidate.ID] = hint
			p.mu.Unlock()
			p.bans.backfillKeyHint(candidate.ID, hint)
		}
		// clearIfExpired auto-re-enables credentials whose reset time passed.
		if p.bans.clearIfExpired(candidate.ID, now) {
			// Still banned: drop from the candidate list.
			bannedCount++
			continue
		}
		available = append(available, candidate)
	}
	if bannedCount > 0 {
		slog.Info("ark-429-autoban: scheduler pick filtered banned credentials",
			"total_candidates", len(req.Candidates), "banned", bannedCount, "available", len(available))
	}

	// If every candidate is banned, return an error so CPA stops retrying
	// instead of falling back to its own selector (which would pick a
	// banned key, get another 429, and loop through all keys).
	if len(available) == 0 {
		slog.Warn("ark-429-autoban: all candidates banned, refusing to schedule",
			"total_candidates", len(req.Candidates), "banned", bannedCount)
		return nil, fmt.Errorf("all credentials banned")
	}

	// When nothing is banned, return Handled: false so CPA falls back to
	// its own selector (e.g., SessionAffinitySelector), preserving session
	// affinity and other built-in routing behaviors.
	if len(available) == len(req.Candidates) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Pick the available candidate with the highest numeric priority value.
	chosen := available[0]
	for _, c := range available[1:] {
		if c.Priority > chosen.Priority {
			chosen = c
		}
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  chosen.ID,
		Handled: true,
	})
}
