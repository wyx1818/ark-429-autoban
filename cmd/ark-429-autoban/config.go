package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// The config_yaml is a YAML snippet. We do a lightweight parse instead of
// importing a full YAML library (which can conflict with the host's version).
// Expected format:
//
//	config_path: "/CLIProxyAPI/config.yaml"
type pluginConfig struct {
	ConfigPath         string `json:"config_path"`
	FallbackBanMinutes int    `json:"fallback_ban_minutes"`
}

func (p *plugin) configure(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		slog.Warn("ark-429-autoban: failed to parse reconfigure request", "error", err)
		return
	}
	if len(req.ConfigYAML) == 0 {
		return
	}

	// Clear old labels.
	p.mu.Lock()
	p.keyLabels = make(map[string]string)
	p.apiKeys = make(map[string]string)
	p.maskedKeys = make(map[string]string)
	p.arkAuths = make(map[string]bool)
	p.configPath = ""
	p.fallbackBan = defaultFallbackBan
	p.mu.Unlock()

	var cfg pluginConfig
	// Lightweight YAML parse for config_path and fallback_ban_minutes.
	parsePluginConfigYAML(string(req.ConfigYAML), &cfg)

	// Store config_path for later reload.
	if cfg.ConfigPath != "" {
		p.mu.Lock()
		p.configPath = cfg.ConfigPath
		p.mu.Unlock()
	}

	// Apply custom fallback ban duration if provided.
	if cfg.FallbackBanMinutes > 0 {
		p.mu.Lock()
		p.fallbackBan = time.Duration(cfg.FallbackBanMinutes) * time.Minute
		p.mu.Unlock()
	}

	count := 0
	if cfg.ConfigPath != "" {
		count = p.autoComputeKeyLabels(cfg.ConfigPath)
		p.mu.Lock()
		p.scannedKeys = count
		p.mu.Unlock()
	}
	slog.Info("ark-429-autoban: loaded key labels", "count", count, "auto_computed", cfg.ConfigPath != "")

	// Load persisted bans and start background saver.
	persistDir := p.resolvePersistDir()
	p.loadBans(persistDir)
	p.startPersister(persistDir)
}

// parsePluginConfigYAML extracts config_path and fallback_ban_minutes from the plugin config YAML.
func parsePluginConfigYAML(yaml string, cfg *pluginConfig) {
	lines := strings.Split(yaml, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "config_path:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "config_path:"))
			// Strip quotes
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') ||
					(val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
			cfg.ConfigPath = val
		}
		if strings.HasPrefix(trimmed, "fallback_ban_minutes:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "fallback_ban_minutes:"))
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				cfg.FallbackBanMinutes = n
			}
		}
	}
}

// autoComputeKeyLabels reads the CPA config file, computes auth IDs for all
// openai-compatibility keys whose base-url uses the official ARK host, and stores
// auth_id => comment mappings.
// Returns the number of labels computed.
func (p *plugin) autoComputeKeyLabels(configPath string) int {
	data, err := p.readFile(configPath)
	if err != nil {
		slog.Warn("ark-429-autoban: failed to read CPA config for auto-compute",
			"path", configPath, "error", err)
		return 0
	}

	content := string(data)
	lines := strings.Split(content, "\n")

	// Pick up the host's routing config so the plugin's ban-filtered
	// scheduling follows the same strategy and affinity the user configured.
	routing := parseRoutingConfig(lines)
	p.mu.Lock()
	// Reset to CPA defaults first so removed keys fall back instead of
	// keeping stale values from the previous load.
	p.strategy = strategyRoundRobin
	p.affinityTTL = time.Hour
	if routing.strategy != "" {
		p.strategy = normalizeStrategy(routing.strategy)
	}
	p.affinity = routing.affinity
	if routing.ttl > 0 {
		p.affinityTTL = routing.ttl
	}
	strategy, affinity, affinityTTL := p.strategy, p.affinity, p.affinityTTL
	p.mu.Unlock()
	slog.Info("ark-429-autoban: following CPA routing config",
		"strategy", strategy, "session_affinity", affinity, "affinity_ttl", affinityTTL)

	currentProvider := ""
	currentBase := ""
	keyIndex := 0 // 1-based per provider, reset on provider change
	count := 0
	inOpenAICompat := false

	for _, line := range lines {
		// Track the top-level openai-compatibility block: provider names are
		// only meaningful inside it (model entries also use "- name:").
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' {
			trimmed := strings.TrimSpace(line)
			inOpenAICompat = trimmed == "openai-compatibility:" || strings.HasPrefix(trimmed, "openai-compatibility: ")
			currentProvider = ""
			currentBase = ""
			continue
		}
		if !inOpenAICompat {
			continue
		}
		// Detect provider name: entries are exactly one level deep
		// ("  - name:"), model list entries are deeper and ignored here.
		if strings.HasPrefix(line, "  - name:") {
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- name:"))
			rest = strings.Trim(rest, "\"' ")
			if rest != "" {
				currentProvider = strings.ToLower(rest)
				currentBase = ""
				keyIndex = 0 // reset for new provider
			}
			continue
		}
		// Detect base-url
		if strings.Contains(line, "base-url:") && currentProvider != "" {
			idx := strings.Index(line, "base-url:")
			rest := strings.TrimSpace(line[idx+9:])
			rest = strings.Trim(rest, "\"' ")
			if rest != "" {
				currentBase = rest
			}
		}
		// Detect api-key (with or without comment)
		if strings.Contains(line, "api-key:") && currentProvider != "" {
			keyIdx := strings.Index(line, "api-key:")
			rest := line[keyIdx+8:]
			key := strings.TrimSpace(rest)
			comment := ""
			if commentIdx := strings.Index(rest, "#"); commentIdx >= 0 {
				key = strings.TrimSpace(rest[:commentIdx])
				comment = strings.TrimSpace(rest[commentIdx+1:])
			}
			if key == "" {
				continue
			}
			keyIndex++

			// Compute auth ID: SHA256(idKind + \0 + key + \0 + base + \0 + proxyURL)[:12]
			idKind := "openai-compatibility:" + currentProvider
			proxyURL := "" // per-key proxy-url is typically empty
			h := sha256.New()
			h.Write([]byte(idKind))
			h.Write([]byte{0})
			h.Write([]byte(key))
			h.Write([]byte{0})
			h.Write([]byte(currentBase))
			h.Write([]byte{0})
			h.Write([]byte(proxyURL))
			short := hex.EncodeToString(h.Sum(nil))[:12]
			authID := idKind + ":" + short

			// Skip providers whose base-url doesn't match the ARK endpoint.
			if !isARKBaseURL(currentBase) {
				continue
			}

			// Use comment if available, otherwise abbreviate the key.
			label := abbreviateKey(key)
			if comment != "" {
				label = comment
			}
			p.mu.Lock()
			p.keyLabels[authID] = label
			p.apiKeys[authID] = fmt.Sprintf("%s #%d", currentProvider, keyIndex)
			p.maskedKeys[authID] = abbreviateKey(key)
			p.arkAuths[authID] = true
			p.mu.Unlock()
			count++
		}
	}

	slog.Info("ark-429-autoban: auto-computed key labels from CPA config",
		"path", configPath, "count", count)
	return count
}

// routingConfig mirrors the CPA routing: block fields this plugin follows.
type routingConfig struct {
	strategy string
	affinity bool
	ttl      time.Duration
}

// parseRoutingConfig extracts strategy / session-affinity / session-affinity-ttl
// from the top-level `routing:` block of the CPA config. Lightweight line-based
// parse (consistent with the rest of this file; avoids a yaml dependency clash
// with the host). Zero values mean "not configured".
func parseRoutingConfig(lines []string) routingConfig {
	var cfg routingConfig
	inRouting := false
	for _, line := range lines {
		// Top-level keys have no leading whitespace; entering `routing:` opens
		// the block, any other top-level key closes it.
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' {
			trimmed := strings.TrimSpace(line)
			inRouting = trimmed == "routing:" || strings.HasPrefix(trimmed, "routing: ")
			continue
		}
		if !inRouting {
			continue
		}
		trimmed := strings.TrimSpace(line)
		key, val, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		if commentIdx := strings.Index(val, " #"); commentIdx >= 0 {
			val = strings.TrimSpace(val[:commentIdx])
		}
		val = strings.Trim(val, "\"'")
		switch strings.TrimSpace(key) {
		case "strategy":
			cfg.strategy = val
		case "session-affinity":
			cfg.affinity = val == "true"
		case "session-affinity-ttl":
			if parsed, err := time.ParseDuration(val); err == nil && parsed > 0 {
				cfg.ttl = parsed
			}
		}
	}
	return cfg
}
