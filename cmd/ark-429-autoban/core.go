package main

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	pluginName            = "ark-429-autoban"
	pluginVersion         = "0.2.1"
	openaiCompatPrefix    = "openai-compatibility:"
	arkHost               = "ark.cn-beijing.volces.com"
	statusTooManyRequests = 429
	defaultFallbackBan    = 30 * time.Minute
	managementRoutePrefix = "/plugins/" + pluginName
)

// Scheduling strategies mirrored from CPA's routing.strategy
// (internal/config RoutingConfig). The plugin replicates them because the
// scheduler plugin ABI cannot delegate filtered candidate sets to the host.
const (
	strategyRoundRobin         = "round-robin"
	strategyWeightedRoundRobin = "weighted-round-robin"
	strategyFillFirst          = "fill-first"
)

// normalizeStrategy maps CPA's accepted aliases to canonical strategy names,
// mirroring sdk/cliproxy/service_config.go. Unknown/empty values fall back to
// round-robin, same as the host default.
func normalizeStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return strategyWeightedRoundRobin
	case "fill-first", "fillfirst", "ff":
		return strategyFillFirst
	default:
		return strategyRoundRobin
	}
}

var resetTimeRe = regexp.MustCompile(`It will reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4} \w+)`)

type plugin struct {
	bans          banState
	mu            sync.RWMutex
	keyHints      map[string]string
	keyLabels     map[string]string
	apiKeys       map[string]string
	maskedKeys    map[string]string
	arkAuths      map[string]bool
	wrr           *wrrScheduler
	rrCursors     map[string]int
	strategy      string
	affinity      bool
	affinityTTL   time.Duration
	affinityStore *affinityStore
	configPath    string
	scannedKeys   int
	fallbackBan   time.Duration
	dirty         chan struct{}
	now           func() time.Time
	readFile      func(string) ([]byte, error)
}

func newPlugin() *plugin {
	return &plugin{
		keyHints:      map[string]string{},
		keyLabels:     map[string]string{},
		apiKeys:       map[string]string{},
		maskedKeys:    map[string]string{},
		arkAuths:      map[string]bool{},
		wrr:           newWRRScheduler(),
		rrCursors:     map[string]int{},
		strategy:      strategyRoundRobin,
		affinityTTL:   time.Hour, // CPA's default when session-affinity-ttl is unset
		affinityStore: newAffinityStore(),
		fallbackBan:   defaultFallbackBan,
		dirty:         make(chan struct{}, 1),
		now:           time.Now,
		readFile:      os.ReadFile,
	}
}

var defaultPlugin = newPlugin()

type banState struct {
	mu   sync.Mutex
	bans map[string]banEntry
}

type banEntry struct {
	ResetAt   time.Time
	Window    string
	BannedAt  time.Time
	KeyHint   string
	ErrorCode string
}

func (s *banState) lookup(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.bans[authID]
	return e, ok
}

func (s *banState) set(authID string, e banEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	s.bans[authID] = e
}

// backfillKeyHint updates the KeyHint of an existing ban entry if it's empty.
// Called from the scheduler hook when it sees a candidate's api_key attribute.
func (s *banState) backfillKeyHint(authID, hint string) {
	if hint == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.bans[authID]
	if !ok || e.KeyHint != "" {
		return
	}
	e.KeyHint = hint
	s.bans[authID] = e
	slog.Info("ark-429-autoban: backfilled key hint for banned credential",
		"auth_id", authID, "key_hint", hint)
}

func (s *banState) clearIfExpired(authID string, now time.Time) (stillBanned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.bans[authID]
	if !ok {
		return false
	}
	if !now.Before(e.ResetAt) {
		delete(s.bans, authID)
		slog.Info("ark-429-autoban: auto re-enabled credential",
			"auth_id", authID, "window", e.Window, "reset_at", e.ResetAt.Format(time.RFC3339))
		return false
	}
	return true
}

func (s *banState) clearExpired(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for authID, e := range s.bans {
		if !now.Before(e.ResetAt) {
			delete(s.bans, authID)
			removed++
			slog.Info("ark-429-autoban: auto re-enabled credential",
				"auth_id", authID, "window", e.Window, "reset_at", e.ResetAt.Format(time.RFC3339))
		}
	}
	return removed
}

func (s *banState) clear(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		return banEntry{}, false
	}
	e, ok := s.bans[authID]
	if ok {
		delete(s.bans, authID)
	}
	return e, ok
}

func (s *banState) clearAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.bans)
	s.bans = make(map[string]banEntry)
	return n
}

func (s *banState) snapshot() map[string]banEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]banEntry, len(s.bans))
	for authID, e := range s.bans {
		out[authID] = e
	}
	return out
}

// errorCodeFallback returns a fallback ban duration for a given ARK error code.
// Returns 0 if the code is unknown (caller should use the generic fallback).
func errorCodeFallback(code string) time.Duration {
	switch code {
	case "ServerOverloaded":
		return 5 * time.Minute
	case "RateLimitExceeded", "Throttled", "RequestLimitExceeded":
		return 10 * time.Minute
	default:
		return 0
	}
}
