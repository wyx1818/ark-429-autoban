package main

import (
	"encoding/json"
	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// --- Management API ---

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodGet,
				Path:        managementRoutePrefix + "/bans",
				Description: "List ARK auths currently held out of the pool by ark-429-autoban.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/unban",
				Description: "Remove one auth from the in-memory ban list. Body: {\"auth_id\":\"...\"}.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/unban-all",
				Description: "Remove every auth from the in-memory ban list.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/reload-config",
				Description: "Reload key labels from the CPA config file.",
			},
		},
		// Resource routes: passive static assets only (no auth, GET-only).
		// Dynamic state and mutations live under the authenticated
		// management routes above.
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "ARK 429 Autoban",
				Description: "View and manually unban ARK credentials after a quota reset.",
			},
			{
				Path:        "/status.css",
				Menu:        "",
				Description: "Embedded stylesheet for the status page.",
			},
			{
				Path:        "/status.js",
				Menu:        "",
				Description: "Embedded script for the status page.",
			},
		},
	}
}

// reloadKeyLabels re-reads the CPA config file and recomputes key labels.
func (p *plugin) reloadKeyLabels() int {
	p.mu.RLock()
	cfgPath := p.configPath
	p.mu.RUnlock()
	if cfgPath == "" {
		return 0
	}
	p.mu.Lock()
	p.keyLabels = make(map[string]string)
	p.apiKeys = make(map[string]string)
	p.maskedKeys = make(map[string]string)
	p.arkAuths = make(map[string]bool)
	p.mu.Unlock()
	count := p.autoComputeKeyLabels(cfgPath)
	p.mu.Lock()
	p.scannedKeys = count
	p.mu.Unlock()
	return count
}

func (p *plugin) handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return okEnvelope(p.dispatchManagement(req))
}

func (p *plugin) dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case method == http.MethodGet && matchesManagementPath(req.Path, "/bans"):
		return jsonManagementResponse(http.StatusOK, p.currentBanStatus())
	case method == http.MethodPost && matchesManagementPath(req.Path, "/unban"):
		return p.handleManagementUnban(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/unban-all"):
		return p.handleManagementUnbanAll()
	case method == http.MethodPost && matchesManagementPath(req.Path, "/reload-config"):
		count := p.reloadKeyLabels()
		return jsonManagementResponse(http.StatusOK, map[string]any{
			"ok":           true,
			"reloaded":     count,
			"scanned_keys": count,
		})
	case method == http.MethodGet && matchesResourcePath(req.Path, "/status"):
		return p.statusPageResponse()
	case method == http.MethodGet && matchesResourcePath(req.Path, "/status.css"):
		return embeddedAssetResponse("web/status.css", "text/css; charset=utf-8")
	case method == http.MethodGet && matchesResourcePath(req.Path, "/status.js"):
		return embeddedAssetResponse("web/status.js", "text/javascript; charset=utf-8")
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]any{
			"error":  "not_found",
			"path":   req.Path,
			"method": method,
		})
	}
}

type managementBanStatus struct {
	Plugin      string              `json:"plugin"`
	Version     string              `json:"version"`
	Count       int                 `json:"count"`
	ScannedKeys int                 `json:"scanned_keys"`
	Bans        []managementBanInfo `json:"bans"`
}

type managementBanInfo struct {
	AuthID           string `json:"auth_id"`
	APIKey           string `json:"api_key"`
	KeyHint          string `json:"key_hint,omitempty"`
	MaskedKey        string `json:"masked_key,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	Window           string `json:"window"`
	BannedAt         string `json:"banned_at,omitempty"`
	BannedAtUnix     int64  `json:"banned_at_unix,omitempty"`
	ResetAt          string `json:"reset_at"`
	ResetAtUnix      int64  `json:"reset_at_unix"`
	RemainingSeconds int64  `json:"remaining_seconds"`
}

func (p *plugin) currentBanStatus() managementBanStatus {
	now := p.now()
	if removed := p.bans.clearExpired(now); removed > 0 {
		p.markDirty()
	}
	snapshot := p.bans.snapshot()
	bans := make([]managementBanInfo, 0, len(snapshot))
	for authID, entry := range snapshot {
		remaining := int64(0)
		if now.Before(entry.ResetAt) {
			remaining = int64(entry.ResetAt.Sub(now).Seconds())
		}
		apiKeyLabel := strings.TrimPrefix(authID, "openai-compatibility:")
		p.mu.RLock()
		apiKey := p.apiKeys[authID]
		keyHintLabel := p.keyLabels[authID]
		if keyHintLabel == "" {
			keyHintLabel = apiKey
		}
		maskedKey := p.maskedKeys[authID]
		p.mu.RUnlock()
		if apiKey != "" {
			apiKeyLabel = apiKey
		}
		info := managementBanInfo{
			AuthID:           authID,
			APIKey:           apiKeyLabel,
			KeyHint:          keyHintLabel,
			MaskedKey:        maskedKey,
			ErrorCode:        entry.ErrorCode,
			Window:           entry.Window,
			ResetAt:          entry.ResetAt.Format("2006-01-02 15:04"),
			ResetAtUnix:      entry.ResetAt.Unix(),
			RemainingSeconds: remaining,
		}
		if !entry.BannedAt.IsZero() {
			info.BannedAt = entry.BannedAt.Format("2006-01-02 15:04")
			info.BannedAtUnix = entry.BannedAt.Unix()
		}
		bans = append(bans, info)
	}
	sort.Slice(bans, func(i, j int) bool {
		if bans[i].ResetAtUnix == bans[j].ResetAtUnix {
			return bans[i].AuthID < bans[j].AuthID
		}
		return bans[i].ResetAtUnix < bans[j].ResetAtUnix
	})
	p.mu.RLock()
	scanned := p.scannedKeys
	p.mu.RUnlock()
	return managementBanStatus{
		Plugin:      pluginName,
		Version:     pluginVersion,
		Count:       len(bans),
		ScannedKeys: scanned,
		Bans:        bans,
	}
}

type managementUnbanRequest struct {
	AuthID string `json:"auth_id"`
	All    bool   `json:"all"`
}

func (p *plugin) handleManagementUnban(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body managementUnbanRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{
				"error":   "invalid_json",
				"message": errUnmarshal.Error(),
			})
		}
	}
	if strings.EqualFold(req.Query.Get("all"), "true") || body.All {
		return p.handleManagementUnbanAll()
	}

	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(req.Query.Get("auth_id"))
	}
	if authID == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error":   "missing_auth_id",
			"message": "provide auth_id in JSON body or query string",
		})
	}

	entry, removed := p.bans.clear(authID)
	if removed {
		p.markDirty()
		slog.Info("ark-429-autoban: manually re-enabled credential",
			"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"auth_id": authID,
		"removed": removed,
		"status":  p.currentBanStatus(),
	})
}

func (p *plugin) handleManagementUnbanAll() pluginapi.ManagementResponse {
	removed := p.bans.clearAll()
	if removed > 0 {
		p.markDirty()
		slog.Info("ark-429-autoban: manually re-enabled all credentials", "removed", removed)
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"removed": removed,
		"status":  p.currentBanStatus(),
	})
}

func matchesManagementPath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	want := managementRoutePrefix + suffix
	return path == want || path == "/v0/management"+want
}

func matchesResourcePath(path, suffix string) bool {
	path = strings.TrimSpace(path)
	// Strip query string (e.g. ?v=2 for cache busting).
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return path == "/v0/resource/plugins/"+pluginName+suffix ||
		path == "/plugins/"+pluginName+suffix
}

func jsonManagementResponse(status int, v any) pluginapi.ManagementResponse {
	raw, errMarshal := json.MarshalIndent(v, "", "  ")
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(map[string]any{
			"error":   "marshal_error",
			"message": errMarshal.Error(),
		})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
		Body: raw,
	}
}
