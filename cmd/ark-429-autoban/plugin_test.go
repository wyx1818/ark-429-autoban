package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
)

func fixedPlugin(now time.Time) *plugin {
	p := newPlugin()
	p.now = func() time.Time { return now }
	return p
}

func envelopeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("unexpected error envelope: %s", raw)
	}
	var result T
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestARKErrorParsing(t *testing.T) {
	want := time.Date(2026, 8, 3, 23, 59, 59, 0, time.FixedZone("CST", 8*3600))
	tests := []struct {
		name, body, window string
		ok                 bool
	}{
		{"monthly JSON", `{"error":{"code":"AccountQuotaExceeded","message":"You have exceeded the monthly usage quota. It will reset at 2026-08-03 23:59:59 +0800 CST."}}`, "monthly", true},
		{"five hour JSON", `{"error":{"type":"TooManyRequests","message":"You have exceeded the 5-hour usage quota. It will reset at 2026-08-03 23:59:59 +0800 CST."}}`, "5-hour", true},
		{"raw body", "It will reset at 2026-08-03 23:59:59 +0800 CST", "", true},
		{"missing reset", `{"error":{"code":"ServerOverloaded","message":"busy"}}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := classifyAndBuildBan(tt.body)
			if ok != tt.ok {
				t.Fatalf("ok=%v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if !entry.ResetAt.Equal(want) {
				t.Fatalf("reset=%s, want %s", entry.ResetAt, want)
			}
			if entry.Window != tt.window {
				t.Fatalf("window=%q, want %q", entry.Window, tt.window)
			}
		})
	}
	if got := extractErrorCode(`{"error":{"code":"ServerOverloaded"}}`); got != "ServerOverloaded" {
		t.Fatalf("code=%q", got)
	}
}

func TestUsageFallbackBan(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	p := fixedPlugin(now)
	authID := "openai-compatibility:ark-code:abc"
	p.mu.Lock()
	p.arkAuths[authID] = true
	p.mu.Unlock()
	record := pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"code":"ServerOverloaded"}}`}}
	raw, _ := json.Marshal(record)
	if _, err := p.handleUsage(raw); err != nil {
		t.Fatal(err)
	}
	entry, ok := p.bans.lookup(record.AuthID)
	if !ok {
		t.Fatal("credential was not banned")
	}
	if got, want := entry.ResetAt, now.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("reset=%s, want %s", got, want)
	}
	if entry.Window != "5m (fallback)" {
		t.Fatalf("window=%q", entry.Window)
	}
	if entry.ErrorCode != "ServerOverloaded" {
		t.Fatalf("error_code=%q", entry.ErrorCode)
	}
}

func TestUsageFallbackByErrorCode(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		body     string
		wantMins int
		wantCode string
	}{
		{
			"ServerOverloaded 5min",
			`{"error":{"code":"ServerOverloaded","message":"busy"}}`,
			5, "ServerOverloaded",
		},
		{
			"RateLimitExceeded 10min",
			`{"error":{"code":"RateLimitExceeded","message":"too fast"}}`,
			10, "RateLimitExceeded",
		},
		{
			"Throttled 10min",
			`{"error":{"code":"Throttled","message":"slow down"}}`,
			10, "Throttled",
		},
		{
			"unknown code uses generic 30min",
			`{"error":{"code":"MysteryError","message":"???"}}`,
			30, "MysteryError",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fixedPlugin(now)
			authID := "openai-compatibility:ark-code:test"
			p.mu.Lock()
			p.arkAuths[authID] = true
			p.mu.Unlock()
			record := pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: tt.body}}
			raw, _ := json.Marshal(record)
			if _, err := p.handleUsage(raw); err != nil {
				t.Fatal(err)
			}
			entry, ok := p.bans.lookup(authID)
			if !ok {
				t.Fatal("not banned")
			}
			if got, want := entry.ResetAt, now.Add(time.Duration(tt.wantMins)*time.Minute); !got.Equal(want) {
				t.Fatalf("reset=%s, want %s", got, want)
			}
			if entry.ErrorCode != tt.wantCode {
				t.Fatalf("error_code=%q, want %q", entry.ErrorCode, tt.wantCode)
			}
		})
	}
}

func TestBanExtendNotShorten(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	p := fixedPlugin(now)
	authID := "openai-compatibility:ark-code:ext"
	p.mu.Lock()
	p.arkAuths[authID] = true
	p.mu.Unlock()

	// First 429: ServerOverloaded -> 5min ban.
	rec1 := pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"code":"ServerOverloaded"}}`}}
	raw1, _ := json.Marshal(rec1)
	p.handleUsage(raw1)

	entry1, ok := p.bans.lookup(authID)
	if !ok {
		t.Fatal("not banned after first 429")
	}
	if got, want := entry1.ResetAt, now.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("first ban reset=%s, want %s", got, want)
	}

	// Second 429: AccountQuotaExceeded with reset at 2 hours later -> should extend.
	futureReset := now.Add(2 * time.Hour)
	resetStr := futureReset.Format("2006-01-02 15:04:05 -0700 MST")
	rec2 := pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"code":"AccountQuotaExceeded","message":"It will reset at ` + resetStr + `"}}`}}
	raw2, _ := json.Marshal(rec2)
	p.handleUsage(raw2)

	entry2, ok := p.bans.lookup(authID)
	if !ok {
		t.Fatal("not banned after second 429")
	}
	if !entry2.ResetAt.Equal(futureReset) {
		t.Fatalf("extended ban reset=%s, want %s", entry2.ResetAt, futureReset)
	}
	// BannedAt should be preserved from original ban.
	if !entry2.BannedAt.Equal(now) {
		t.Fatalf("banned_at=%s, want %s (should preserve original)", entry2.BannedAt, now)
	}

	// Third 429: ServerOverloaded (5min) -> should NOT shorten the 2h ban.
	rec3 := pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: `{"error":{"code":"ServerOverloaded"}}`}}
	raw3, _ := json.Marshal(rec3)
	p.handleUsage(raw3)

	entry3, ok := p.bans.lookup(authID)
	if !ok {
		t.Fatal("not banned after third 429")
	}
	if !entry3.ResetAt.Equal(futureReset) {
		t.Fatalf("ban was shortened! reset=%s, should still be %s", entry3.ResetAt, futureReset)
	}
}

func TestBanStateLifecycleAndConcurrency(t *testing.T) {
	var state banState
	now := time.Now()
	state.set("expired", banEntry{ResetAt: now.Add(-time.Second)})
	state.set("active", banEntry{ResetAt: now.Add(time.Hour)})
	if removed := state.clearExpired(now); removed != 1 {
		t.Fatalf("removed=%d", removed)
	}
	if state.clearIfExpired("active", now) != true {
		t.Fatal("active ban should remain")
	}
	if _, ok := state.clear("active"); !ok {
		t.Fatal("active ban not cleared")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); state.set("shared", banEntry{ResetAt: now.Add(time.Hour)}); state.snapshot() }()
	}
	wg.Wait()
	if n := state.clearAll(); n != 1 {
		t.Fatalf("clearAll=%d", n)
	}
}

func TestSchedulerBehavior(t *testing.T) {
	now := time.Now()
	ark1 := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:a", Priority: 1}
	ark2 := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:b", Priority: 9}
	other := pluginapi.SchedulerAuthCandidate{ID: "codex:c", Priority: 3}
	tests := []struct {
		name       string
		candidates []pluginapi.SchedulerAuthCandidate
		banned     []string
		handled    bool
		authID     string
		wantErr    bool
	}{
		{"none banned delegates", []pluginapi.SchedulerAuthCandidate{ark1, ark2}, nil, false, "", false},
		{"partial picks highest", []pluginapi.SchedulerAuthCandidate{ark1, ark2, other}, []string{ark2.ID}, true, other.ID, false},
		{"all banned errors", []pluginapi.SchedulerAuthCandidate{ark1, ark2}, []string{ark1.ID, ark2.ID}, false, "", true},
		{"non ARK delegates", []pluginapi.SchedulerAuthCandidate{other}, nil, false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fixedPlugin(now)
			p.mu.Lock()
			p.arkAuths[ark1.ID] = true
			p.arkAuths[ark2.ID] = true
			p.mu.Unlock()
			for _, id := range tt.banned {
				p.bans.set(id, banEntry{ResetAt: now.Add(time.Hour)})
			}
			raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Candidates: tt.candidates})
			out, err := p.handleSchedulerPick(raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := envelopeResult[pluginapi.SchedulerPickResponse](t, out)
			if got.Handled != tt.handled || got.AuthID != tt.authID {
				t.Fatalf("got=%+v", got)
			}
		})
	}
}

func TestIsARKBaseURL(t *testing.T) {
	tests := []struct {
		name, raw string
		want      bool
	}{
		{"host only", "https://ark.cn-beijing.volces.com", true},
		{"plan endpoint", "https://ark.cn-beijing.volces.com/api/plan/v3", true},
		{"coding endpoint", "https://ark.cn-beijing.volces.com/api/coding/v3/", true},
		{"uppercase host", "https://ARK.CN-BEIJING.VOLCES.COM/api/plan/v3", true},
		{"surrounding whitespace", "  https://ark.cn-beijing.volces.com/api/plan/v3  ", true},
		{"http rejected", "http://ark.cn-beijing.volces.com/api/plan/v3", false},
		{"lookalike suffix", "https://ark.cn-beijing.volces.com.evil.example/api/plan/v3", false},
		{"subdomain", "https://proxy.ark.cn-beijing.volces.com/api/plan/v3", false},
		{"userinfo", "https://user@ark.cn-beijing.volces.com/api/plan/v3", false},
		{"custom port", "https://ark.cn-beijing.volces.com:443/api/plan/v3", false},
		{"not a URL", "ark.cn-beijing.volces.com/api/plan/v3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isARKBaseURL(tt.raw); got != tt.want {
				t.Fatalf("isARKBaseURL(%q)=%v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestConfigAutoCompute(t *testing.T) {
	p := newPlugin()
	p.readFile = func(path string) ([]byte, error) {
		if path == "/CLIProxyAPI/config.yaml" {
			return []byte("name: ark-code\nbase-url: https://ark.cn-beijing.volces.com/api/coding/v3\napi-key: arksk-...5678 # automatic\n"), nil
		}
		// bans file doesn't exist during tests.
		return nil, os.ErrNotExist
	}
	configYAML := "config_path: /CLIProxyAPI/config.yaml\n"
	req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(configYAML)})
	p.configure(req)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.configPath != "/CLIProxyAPI/config.yaml" {
		t.Fatalf("configPath=%q", p.configPath)
	}
	if len(p.apiKeys) != 1 {
		t.Fatalf("apiKeys=%d", len(p.apiKeys))
	}
	for authID := range p.apiKeys {
		if p.keyLabels[authID] != "automatic" {
			t.Fatalf("auto label=%q", p.keyLabels[authID])
		}
		if !p.arkAuths[authID] {
			t.Fatal("auth ID not in arkAuths")
		}
	}
}

func TestConfigSkipsNonARKBaseURL(t *testing.T) {
	p := newPlugin()
	p.readFile = func(path string) ([]byte, error) {
		if path == "/CLIProxyAPI/config.yaml" {
			return []byte("name: ark-code\nbase-url: https://example.com\napi-key: arksk-...5678 # other\n"), nil
		}
		return nil, os.ErrNotExist
	}
	configYAML := "config_path: /CLIProxyAPI/config.yaml\n"
	req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(configYAML)})
	p.configure(req)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.apiKeys) != 0 {
		t.Fatalf("expected 0 apiKeys for non-ARK base-url, got %d", len(p.apiKeys))
	}
	if len(p.arkAuths) != 0 {
		t.Fatalf("expected 0 arkAuths for non-ARK base-url, got %d", len(p.arkAuths))
	}
}

func TestManagementAndEmbeddedWeb(t *testing.T) {
	now := time.Now()
	p := fixedPlugin(now)
	authID := `openai-compatibility:ark-code:<script>alert(1)</script>`
	p.bans.set(authID, banEntry{ResetAt: now.Add(time.Hour), Window: `<b>monthly</b>`})
	p.mu.Lock()
	p.keyLabels[authID] = `<img src=x onerror=alert(1)>`
	p.mu.Unlock()

	status := p.currentBanStatus()
	if status.Count != 1 || status.Bans[0].AuthID != authID {
		t.Fatalf("status=%+v", status)
	}
	page := p.dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/status"})
	if page.StatusCode != 200 || !strings.HasPrefix(page.Headers.Get("Content-Type"), "text/html") {
		t.Fatalf("page=%+v", page)
	}
	if strings.Contains(string(page.Body), `<script>alert(1)</script>`) || !strings.Contains(string(page.Body), `&lt;script&gt;`) {
		t.Fatal("status template did not escape dynamic content")
	}
	for _, tc := range []struct{ path, contentType string }{{"/status.css", "text/css"}, {"/status.js", "text/javascript"}} {
		resp := p.dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + tc.path})
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Headers.Get("Content-Type"), tc.contentType) || len(resp.Body) == 0 {
			t.Fatalf("asset %s failed", tc.path)
		}
	}
	bad := p.dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRoutePrefix + "/unban", Body: []byte("{")})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad status=%d", bad.StatusCode)
	}
	// Unauthenticated resource-path mutations must no longer exist.
	for _, legacy := range []string{"/unban", "/unban-all", "/reload-config", "/bans.json"} {
		resp := p.dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + legacy})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("legacy resource route %s still served (status=%d)", legacy, resp.StatusCode)
		}
	}
	unban := p.dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRoutePrefix + "/unban", Body: []byte(`{"auth_id":"` + authID + `"}`)})
	if unban.StatusCode != 200 {
		t.Fatalf("unban status=%d", unban.StatusCode)
	}
	if _, ok := p.bans.lookup(authID); ok {
		t.Fatal("ban still present")
	}
	reload := p.dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRoutePrefix + "/reload-config"})
	if reload.StatusCode != 200 {
		t.Fatalf("reload status=%d", reload.StatusCode)
	}
}

func TestManagementRegistrationIncludesAssets(t *testing.T) {
	registration := managementRegistration()
	want := map[string]bool{"/status": false, "/status.css": false, "/status.js": false}
	for _, route := range registration.Resources {
		if _, ok := want[route.Path]; ok {
			want[route.Path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("missing resource route %s", path)
		}
	}
	// Dynamic routes must be registered as authenticated management routes.
	mgmtWant := map[string]bool{
		managementRoutePrefix + "/bans":           false,
		managementRoutePrefix + "/unban":          false,
		managementRoutePrefix + "/unban-all":      false,
		managementRoutePrefix + "/reload-config":  false,
	}
	for _, route := range registration.Routes {
		if _, ok := mgmtWant[route.Path]; ok {
			mgmtWant[route.Path] = true
		}
	}
	for path, found := range mgmtWant {
		if !found {
			t.Errorf("missing management route %s", path)
		}
	}
}
