package main

import (
	"strings"
	"sync"
	"time"

	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
)

// Session affinity replicated plugin-side. CPA's SessionAffinitySelector lives
// in m.selector, which is bypassed whenever this plugin handles a pick (the
// only way to exclude banned keys). CPA's session.Enrich runs before
// scheduling and already derives a stable session identity from the payload,
// so the plugin can rebuild bindings from SchedulerPickRequest.Options alone.
//
// Extraction priority mirrors extractSessionIDs (selector.go), minus the
// body-based IDs the scheduler ABI does not expose.
var explicitSessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"Session-Id",
	"Session_id",
	"X-Session-ID",
	"X-Session-Affinity",
	"X-Client-Request-Id",
}

// extractSessionID returns a stable session identity for the request, or ""
// when none can be determined (callers then skip affinity and just run the
// configured strategy, same as CPA's fallback).
func extractSessionID(opts pluginapi.SchedulerOptions) string {
	for _, header := range explicitSessionHeaders {
		for key, values := range opts.Headers {
			if !strings.EqualFold(key, header) {
				continue
			}
			for _, v := range values {
				if v = strings.TrimSpace(v); v != "" {
					return "hdr:" + strings.ToLower(header) + ":" + v
				}
			}
		}
	}
	if opts.Metadata != nil {
		if v, ok := opts.Metadata["execution_session_id"].(string); ok && strings.TrimSpace(v) != "" {
			return "execution:" + strings.TrimSpace(v)
		}
		if v, ok := opts.Metadata["derived_session_id"].(string); ok && strings.TrimSpace(v) != "" {
			return "derived:" + strings.TrimSpace(v)
		}
	}
	return ""
}

type affinityBinding struct {
	authID    string
	expiresAt time.Time
}

// affinityStore keeps session -> auth bindings with TTL, mirroring CPA's
// SessionCache semantics (provider::session::model namespace, sliding TTL
// refreshed on each affinity hit).
type affinityStore struct {
	mu       sync.Mutex
	bindings map[string]affinityBinding
}

func newAffinityStore() *affinityStore {
	return &affinityStore{bindings: map[string]affinityBinding{}}
}

// get returns the bound auth ID when the binding exists and is still valid.
func (s *affinityStore) get(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[key]
	if !ok {
		return "", false
	}
	if !binding.expiresAt.After(now) {
		delete(s.bindings, key)
		return "", false
	}
	return binding.authID, true
}

func (s *affinityStore) set(key, authID string, ttl time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound memory the same way CPA caps selector state maps.
	if _, ok := s.bindings[key]; !ok && len(s.bindings) >= 4096 {
		s.bindings = map[string]affinityBinding{}
	}
	s.bindings[key] = affinityBinding{authID: authID, expiresAt: now.Add(ttl)}
}

// touch extends an existing binding's TTL, mirroring CPA's SessionCache.Touch
// on successful results. The plugin cannot observe per-request success (usage
// records carry no session ID), so it touches at pick time instead: an active
// session keeps its binding alive, an idle one expires after the TTL.
func (s *affinityStore) touch(key string, ttl time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[key]
	if !ok {
		return
	}
	binding.expiresAt = now.Add(ttl)
	s.bindings[key] = binding
}
