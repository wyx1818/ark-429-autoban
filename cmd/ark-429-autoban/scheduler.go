package main

import (
	"encoding/json"
	"fmt"
	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
	"log/slog"
	"strings"
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

	// CPA groups candidates by priority before calling plugins, but stay
	// defensive: restrict to the highest priority group, then run smooth
	// weighted round-robin inside it (same algorithm as CPA's
	// WeightedRoundRobinSelector; the plugin ABI cannot delegate to it).
	// Always filter to the top tier; 0 is not a sentinel (priorities may be
	// negative or all zero).
	top := available[:0]
	maxPriority := available[0].Priority
	for _, c := range available[1:] {
		if c.Priority > maxPriority {
			maxPriority = c.Priority
		}
	}
	for _, c := range available {
		if c.Priority == maxPriority {
			top = append(top, c)
		}
	}
	stateKey := strings.ToLower(strings.TrimSpace(req.Provider)) + ":" + strings.TrimSpace(req.Model)
	p.mu.RLock()
	strategy := p.strategy
	affinity, affinityTTL := p.affinity, p.affinityTTL
	p.mu.RUnlock()

	// Session affinity: when CPA's affinity is enabled, honor an existing
	// binding if the bound auth survived the ban filter; otherwise fall
	// through to the strategy pick and rebind. This mirrors the host's
	// SessionAffinitySelector, which the plugin pick path bypasses.
	sessionKey := ""
	if affinity {
		if sessionID := extractSessionID(req.Options); sessionID != "" {
			sessionKey = strings.ToLower(strings.TrimSpace(req.Provider)) + "::" + sessionID + "::" + strings.TrimSpace(req.Model)
			if boundAuthID, ok := p.affinityStore.get(sessionKey, now); ok {
				for _, c := range top {
					if c.ID == boundAuthID {
						p.affinityStore.touch(sessionKey, affinityTTL, now)
						slog.Info("ark-429-autoban: scheduler pick (session affinity hit)",
							"auth_id", c.ID, "available", len(available), "banned", bannedCount)
						return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: c.ID, Handled: true})
					}
				}
				slog.Info("ark-429-autoban: session binding no longer available, reselecting",
					"bound_auth_id", boundAuthID, "available", len(available), "banned", bannedCount)
			}
		}
	}

	var chosen pluginapi.SchedulerAuthCandidate
	switch strategy {
	case strategyFillFirst:
		// Mirror FillFirstSelector: always the first available candidate.
		chosen = top[0]
	case strategyWeightedRoundRobin:
		picked, ok := p.wrr.pick(stateKey, top)
		if !ok {
			// Every remaining candidate has a non-positive explicit weight,
			// mirroring CPA's "no auth available with positive weight".
			slog.Warn("ark-429-autoban: no candidate with positive weight, refusing to schedule",
				"total_candidates", len(req.Candidates), "available", len(available))
			return nil, fmt.Errorf("no credential available with positive weight")
		}
		chosen = picked
	default: // strategyRoundRobin
		// Mirror RoundRobinSelector: a per-(provider, model) cursor advanced
		// on every pick, modulo the current candidate count.
		p.mu.Lock()
		index := p.rrCursors[stateKey]
		if index >= 2_147_483_640 {
			index = 0
		}
		p.rrCursors[stateKey] = index + 1
		p.mu.Unlock()
		chosen = top[index%len(top)]
	}
	slog.Info("ark-429-autoban: scheduler pick",
		"strategy", strategy, "auth_id", chosen.ID, "weight", candidateWeight(chosen),
		"group_size", len(top), "available", len(available), "banned", bannedCount)
	if sessionKey != "" {
		p.affinityStore.set(sessionKey, chosen.ID, affinityTTL, now)
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  chosen.ID,
		Handled: true,
	})
}
