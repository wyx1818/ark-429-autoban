package main

import (
	"encoding/json"
	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginabi"
	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
	"net/url"
	"strings"
)

func (p *plugin) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		p.configure(request)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodUsageHandle:
		return p.handleUsage(request)
	case pluginabi.MethodSchedulerPick:
		return p.handleSchedulerPick(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return p.handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "zuiyu",
			GitHubRepository: "https://github.com/wyx1818/ark-429-autoban",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "config_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Path to CPA config.yaml for auto-computing key labels from api-key comments.",
				},
				{
					Name:        "fallback_ban_minutes",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Default ban duration in minutes when 429 body has no parseable reset time. Default: 30.",
				},
			},
		},
		Capabilities: registrationCapability{
			UsagePlugin:   true,
			Scheduler:     true,
			ManagementAPI: true,
		},
	}
}

// isARKCredential checks whether an auth ID belongs to an openai-compatibility
// (ARK) credential by looking for the "openai-compatibility:" prefix.
func isARKCredential(authID string) bool {
	return strings.HasPrefix(authID, openaiCompatPrefix)
}

// isARKBaseURL accepts only HTTPS URLs on the official ARK host. Paths such as
// /api/plan/v3 and /api/coding/v3 are allowed; look-alike hosts are rejected.
func isARKBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), arkHost) && u.Port() == ""
}

func (p *plugin) isARKAuth(authID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.arkAuths[authID]
}

func truncateBody(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// abbreviateKey returns a short hint like "ark-sk...gJtf" for display.
func abbreviateKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "..." + key[len(key)-4:]
}

// buildHintFromAttrs creates a human-readable hint from scheduler candidate
// attributes. First checks if a label is configured for the full auth ID,
// then falls back to provider name.
func (p *plugin) buildHintFromAttrs(authID string, attrs map[string]string) string {
	// Check if a label is configured for the full auth ID.
	p.mu.RLock()
	defer p.mu.RUnlock()
	if s := p.keyLabels[authID]; s != "" {
		return s
	}
	// Fallback: apiKeys has "provider #N" label.
	if s := p.apiKeys[authID]; s != "" {
		return s
	}
	// Fallback: compat_name + provider_key.
	compatName := strings.TrimSpace(attrs["compat_name"])
	providerKey := strings.TrimSpace(attrs["provider_key"])
	if compatName != "" {
		return compatName
	}
	if providerKey != "" {
		return providerKey
	}
	// Last resort: last segment of auth ID
	parts := strings.Split(authID, ":")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}

// keyHintFor returns the cached abbreviated key for an auth ID, or "".
func (p *plugin) keyHintFor(authID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.keyHints[authID]
}

// extractQuotaPeriod extracts a human-readable quota period from an ARK error
// message like "You have exceeded the 5-hour usage quota" or
// "You have exceeded the monthly usage quota".
func extractQuotaPeriod(msg string) string {
	msg = strings.ToLower(msg)
	idx := strings.Index(msg, "exceeded the")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(msg[idx+len("exceeded the"):])
	endIdx := strings.Index(rest, "usage quota")
	if endIdx < 0 {
		endIdx = strings.Index(rest, "quota")
	}
	if endIdx < 0 {
		return ""
	}
	period := strings.TrimSpace(rest[:endIdx])
	if period == "" {
		return ""
	}
	return period
}

// extractErrorCode reads the "code" field from a JSON error body like
// {"error":{"code":"ServerOverloaded",...}}.
func extractErrorCode(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var errObj struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &errObj); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(errObj.Error.Code)
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}
