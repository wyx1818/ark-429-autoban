package main

import (
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
)

// Weight bounds mirrored from CLIProxyAPI's internal/credentialweight package
// so plugin-side scheduling interprets config weights exactly like the host.
const (
	defaultWeight int64 = 1
	maxWeight     int64 = 1_000_000
)

// wrrScheduler is a 1:1 port of CPA's smooth weighted round-robin
// (sdk/cliproxy/auth/selector.go: WeightedRoundRobinSelector /
// pickSmoothWeightedAuth). CPA's plugin ABI cannot delegate to the built-in
// WRR (builtinSchedulerStrategy only maps "round-robin"/"fill-first"), and
// DelegateBuiltin operates on the full unfiltered candidate set, so the
// algorithm is replicated here to run over the ban-filtered candidates.
type wrrScheduler struct {
	mu     sync.Mutex
	states map[string]*smoothWeightedState
}

type smoothWeightedState struct {
	weights map[string]int64
	current map[string]int64
}

func newWRRScheduler() *wrrScheduler {
	return &wrrScheduler{states: map[string]*smoothWeightedState{}}
}

// candidateWeight mirrors CPA's authWeight: explicit weight from attributes,
// absent/empty means default (1), invalid or out of range means 0 (excluded
// from weighted routing, matching credentialweight.Normalize semantics).
func candidateWeight(c pluginapi.SchedulerAuthCandidate) int64 {
	raw, ok := c.Attributes["weight"]
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultWeight
	}
	weight, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || weight > maxWeight {
		return 0
	}
	if weight <= 0 {
		return 0
	}
	return weight
}

// pick selects one candidate using smooth weighted round-robin, keyed by
// stateKey (provider:model) so different models keep independent cursors.
// Returns false when no candidate has a positive weight.
func (s *wrrScheduler) pick(stateKey string, candidates []pluginapi.SchedulerAuthCandidate) (pluginapi.SchedulerAuthCandidate, bool) {
	weights := make(map[string]int64, len(candidates))
	for _, c := range candidates {
		if w := candidateWeight(c); w > 0 {
			weights[c.ID] = w
		}
	}
	if len(weights) == 0 {
		return pluginapi.SchedulerAuthCandidate{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[stateKey]
	if state == nil {
		state = &smoothWeightedState{}
		s.states[stateKey] = state
	}
	// prepare: reset currents when the weight vector changed (ban/unban or
	// config reload), matching CPA's smoothWeightedState.prepare.
	if state.current == nil || !weightVectorsEqual(state.weights, weights) {
		state.current = make(map[string]int64)
	}
	state.weights = weights

	var picked pluginapi.SchedulerAuthCandidate
	var pickedCurrent int64
	var totalWeight int64
	active := make(map[string]struct{}, len(candidates))
	found := false
	for _, c := range candidates {
		weight := candidateWeight(c)
		if weight <= 0 {
			continue
		}
		active[c.ID] = struct{}{}
		state.current[c.ID] = saturatingAdd(state.current[c.ID], weight)
		totalWeight = saturatingAdd(totalWeight, weight)
		if !found || state.current[c.ID] > pickedCurrent {
			picked = c
			pickedCurrent = state.current[c.ID]
			found = true
		}
	}
	for id := range state.current {
		if _, ok := active[id]; !ok {
			delete(state.current, id)
		}
	}
	if !found {
		return pluginapi.SchedulerAuthCandidate{}, false
	}
	state.current[picked.ID] = saturatingAdd(state.current[picked.ID], -totalWeight)
	return picked, true
}

func weightVectorsEqual(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for id, weight := range left {
		if right[id] != weight {
			return false
		}
	}
	return true
}

func saturatingAdd(value, delta int64) int64 {
	if delta > 0 && value > math.MaxInt64-delta {
		return math.MaxInt64
	}
	if delta < 0 && value < math.MinInt64-delta {
		return math.MinInt64
	}
	return value + delta
}
