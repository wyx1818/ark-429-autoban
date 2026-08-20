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
			return []byte("openai-compatibility:\n  - name: \"ark-code\"\n    base-url: https://ark.cn-beijing.volces.com/api/coding/v3\n    api-key-entries:\n      - api-key: arksk-...5678 # automatic\n"), nil
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
			return []byte("openai-compatibility:\n  - name: \"ark-code\"\n    base-url: https://example.com\n    api-key-entries:\n      - api-key: arksk-...5678 # other\n"), nil
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
	pageBody := string(page.Body)
	for _, leak := range []string{
		authID,
		`<img src=x onerror=alert(1)>`,
		`<b>monthly</b>`,
	} {
		if strings.Contains(pageBody, leak) {
			t.Fatalf("unauthenticated status page leaked dynamic ban data: %q", leak)
		}
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
		managementRoutePrefix + "/bans":          false,
		managementRoutePrefix + "/unban":         false,
		managementRoutePrefix + "/unban-all":     false,
		managementRoutePrefix + "/reload-config": false,
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

func TestManagementAndResourcePathMatchingIsExact(t *testing.T) {
	tests := []struct {
		path     string
		suffix   string
		mgmtWant bool
		resource bool
	}{
		{managementRoutePrefix + "/bans", "/bans", true, true},
		{managementRoutePrefix + "/bans/", "/bans", true, true},
		{managementRoutePrefix + "/bans?x=1", "/bans", false, true},
		{"/v0/management/plugins/" + pluginName + "/bans", "/bans", true, false},
		{"/v0/management/plugins/" + pluginName + "/bans/", "/bans", true, false},
		{"/v0/management/plugins/not-" + pluginName + "/bans", "/bans", false, false},
		{"/v0/resource/plugins/" + pluginName + "/status", "/status", false, true},
		{"/v0/resource/plugins/" + pluginName + "/status?cache=1", "/status", false, true},
		{managementRoutePrefix + "/status.js", "/status.js", true, true},
		{"/v0/resource/plugins/" + pluginName + "/status.js", "/status.js", false, true},
		{"/v0/resource/plugins/not-" + pluginName + "/status", "/status", false, false},
	}
	for _, tc := range tests {
		if got := matchesManagementPath(tc.path, tc.suffix); got != tc.mgmtWant {
			t.Errorf("matchesManagementPath(%q, %q) = %v, want %v", tc.path, tc.suffix, got, tc.mgmtWant)
		}
		if got := matchesResourcePath(tc.path, tc.suffix); got != tc.resource {
			t.Errorf("matchesResourcePath(%q, %q) = %v, want %v", tc.path, tc.suffix, got, tc.resource)
		}
	}
}

func TestResourcePathsCannotReachManagementOperations(t *testing.T) {
	now := time.Now()
	p := fixedPlugin(now)
	authID := "openai-compatibility:ark-code:secret"
	p.bans.set(authID, banEntry{ResetAt: now.Add(time.Hour), Window: "monthly"})

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v0/resource/plugins/" + pluginName + "/bans"},
		{http.MethodGet, "/v0/resource/plugins/" + pluginName + "/unban"},
		{http.MethodGet, "/v0/resource/plugins/" + pluginName + "/unban-all"},
		{http.MethodGet, "/v0/resource/plugins/" + pluginName + "/reload-config"},
		{http.MethodPost, "/v0/resource/plugins/" + pluginName + "/unban"},
		{http.MethodPost, "/v0/resource/plugins/" + pluginName + "/unban-all"},
		{http.MethodPost, "/v0/resource/plugins/" + pluginName + "/reload-config"},
	} {
		resp := p.dispatchManagement(pluginapi.ManagementRequest{Method: tc.method, Path: tc.path})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: got status %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
	if _, ok := p.bans.lookup(authID); !ok {
		t.Fatal("resource request mutated ban state")
	}
}

func weighted(id string, weight string) pluginapi.SchedulerAuthCandidate {
	attrs := map[string]string{}
	if weight != "" {
		attrs["weight"] = weight
	}
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: "ark-code", Attributes: attrs}
}

func TestWRRSmoothDistribution(t *testing.T) {
	s := newWRRScheduler()
	candidates := []pluginapi.SchedulerAuthCandidate{
		weighted("a", "2"), weighted("b", "1"), weighted("c", ""), // c defaults to 1
	}
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		picked, ok := s.pick("ark-code:glm-5.2", candidates)
		if !ok {
			t.Fatal("no pick")
		}
		counts[picked.ID]++
	}
	// Smooth WRR over 8 picks with weights 2:1:1 must be exactly 4:2:2.
	if counts["a"] != 4 || counts["b"] != 2 || counts["c"] != 2 {
		t.Fatalf("counts=%v, want a=4 b=2 c=2", counts)
	}
}

func TestWRRZeroAndInvalidWeightExcluded(t *testing.T) {
	s := newWRRScheduler()
	candidates := []pluginapi.SchedulerAuthCandidate{
		weighted("zero", "0"), weighted("neg", "-3"), weighted("bad", "abc"),
		weighted("over", "1000001"), weighted("ok", "1"),
	}
	for i := 0; i < 5; i++ {
		picked, ok := s.pick("k", candidates)
		if !ok || picked.ID != "ok" {
			t.Fatalf("pick=%q ok=%v, want ok only", picked.ID, ok)
		}
	}
	// All non-positive weights: no pick, mirroring CPA's behavior.
	if _, ok := s.pick("k", candidates[:4]); ok {
		t.Fatal("expected no pick when all weights are non-positive")
	}
}

func TestWRRWeightVectorChangeResets(t *testing.T) {
	s := newWRRScheduler()
	two := []pluginapi.SchedulerAuthCandidate{weighted("a", "1"), weighted("b", "1")}
	for i := 0; i < 4; i++ {
		if _, ok := s.pick("k", two); !ok {
			t.Fatal("no pick")
		}
	}
	// b gets banned: only a remains, currents reset, a picked every time.
	one := two[:1]
	for i := 0; i < 3; i++ {
		picked, ok := s.pick("k", one)
		if !ok || picked.ID != "a" {
			t.Fatalf("pick=%q ok=%v", picked.ID, ok)
		}
	}
}

func TestWRRPerModelStateIsolation(t *testing.T) {
	s := newWRRScheduler()
	candidates := []pluginapi.SchedulerAuthCandidate{weighted("a", "1"), weighted("b", "1")}
	first, _ := s.pick("p:model-1", candidates)
	// A different model has its own cursor and must start from a again.
	other, _ := s.pick("p:model-2", candidates)
	if first.ID != other.ID {
		t.Fatalf("model-2 first pick=%q, want same as model-1 first pick %q", other.ID, first.ID)
	}
}

func TestSchedulerWRRSkipsBanned(t *testing.T) {
	now := time.Now()
	p := fixedPlugin(now)
	arkA := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:a", Attributes: map[string]string{"weight": "3"}}
	arkB := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:b", Attributes: map[string]string{"weight": "1"}}
	p.mu.Lock()
	p.arkAuths[arkA.ID] = true
	p.arkAuths[arkB.ID] = true
	p.mu.Unlock()
	p.bans.set(arkA.ID, banEntry{ResetAt: now.Add(time.Hour)})

	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider:   "ark-code",
		Model:      "glm-5.2",
		Candidates: []pluginapi.SchedulerAuthCandidate{arkA, arkB},
	})
	for i := 0; i < 3; i++ {
		out, err := p.handleSchedulerPick(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := envelopeResult[pluginapi.SchedulerPickResponse](t, out)
		if !got.Handled || got.AuthID != arkB.ID {
			t.Fatalf("got=%+v, want handled pick of banned-filtered arkB", got)
		}
	}
}

func TestParseRoutingStrategy(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"wrr", "routing:\n  strategy: weighted-round-robin\n", "weighted-round-robin"},
		{"fill-first quoted", "routing:\n  strategy: \"fill-first\"\n", "fill-first"},
		{"with comment", "routing: # routing config\n  strategy: wrr\n", "wrr"},
		{"absent", "port: 8317\n", ""},
		{"closed by next top key", "routing:\n  strategy: fill-first\ndebug: true\n  strategy: wrr\n", "fill-first"},
		{"comment line not treated as key", "# routing:\nport: 1\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRoutingConfig(strings.Split(tt.yaml, "\n")).strategy; got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	// session-affinity fields
	rc := parseRoutingConfig(strings.Split("routing:\n  strategy: wrr\n  session-affinity: true\n  session-affinity-ttl: 2h\n", "\n"))
	if !rc.affinity || rc.ttl != 2*time.Hour || rc.strategy != "wrr" {
		t.Fatalf("routing config=%+v", rc)
	}
	if rc := parseRoutingConfig(strings.Split("port: 1\n", "\n")); rc.affinity || rc.ttl != 0 {
		t.Fatalf("unexpected affinity defaults=%+v", rc)
	}
	if normalizeStrategy("wrr") != strategyWeightedRoundRobin ||
		normalizeStrategy("FF") != strategyFillFirst ||
		normalizeStrategy("bogus") != strategyRoundRobin ||
		normalizeStrategy("") != strategyRoundRobin {
		t.Fatal("normalizeStrategy alias mapping wrong")
	}
}

// schedulerPickN runs n picks through handleSchedulerPick with the given
// strategy and returns the picked auth IDs in order.
func schedulerPickN(t *testing.T, p *plugin, strategy string, req pluginapi.SchedulerPickRequest, n int) []string {
	t.Helper()
	p.mu.Lock()
	p.strategy = strategy
	p.mu.Unlock()
	raw, _ := json.Marshal(req)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out, err := p.handleSchedulerPick(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := envelopeResult[pluginapi.SchedulerPickResponse](t, out)
		if !got.Handled {
			t.Fatal("expected handled pick")
		}
		ids = append(ids, got.AuthID)
	}
	return ids
}

func TestSchedulerStrategies(t *testing.T) {
	now := time.Now()
	mk := func(id string) pluginapi.SchedulerAuthCandidate {
		return pluginapi.SchedulerAuthCandidate{ID: id, Attributes: map[string]string{"weight": "1"}}
	}
	a, b, c := mk("openai-compatibility:ark-code:a"), mk("openai-compatibility:ark-code:b"), mk("openai-compatibility:ark-code:c")
	req := pluginapi.SchedulerPickRequest{Provider: "ark-code", Model: "glm-5.2", Candidates: []pluginapi.SchedulerAuthCandidate{a, b, c}}

	newP := func() *plugin {
		p := fixedPlugin(now)
		p.mu.Lock()
		p.arkAuths[a.ID], p.arkAuths[b.ID], p.arkAuths[c.ID] = true, true, true
		p.mu.Unlock()
		// Ban one key so the plugin takes over scheduling for every case.
		p.bans.set(c.ID, banEntry{ResetAt: now.Add(time.Hour)})
		return p
	}

	// round-robin rotates over the filtered set (c banned).
	if ids := schedulerPickN(t, newP(), strategyRoundRobin, req, 4); strings.Join(ids, ",") != a.ID+","+b.ID+","+a.ID+","+b.ID {
		t.Fatalf("round-robin ids=%v", ids)
	}
	// fill-first always picks the first available.
	if ids := schedulerPickN(t, newP(), strategyFillFirst, req, 3); strings.Join(ids, ",") != a.ID+","+a.ID+","+a.ID {
		t.Fatalf("fill-first ids=%v", ids)
	}
	// weighted round-robin with equal weights alternates too, over filtered set.
	if ids := schedulerPickN(t, newP(), strategyWeightedRoundRobin, req, 4); strings.Join(ids, ",") != a.ID+","+b.ID+","+a.ID+","+b.ID {
		t.Fatalf("wrr ids=%v", ids)
	}
}

func TestExtractSessionID(t *testing.T) {
	// Header priority and case-insensitivity.
	opts := pluginapi.SchedulerOptions{Headers: map[string][]string{"x-session-id": {"  abc  "}}}
	if got := extractSessionID(opts); got != "hdr:x-session-id:abc" {
		t.Fatalf("header got %q", got)
	}
	// Metadata fallback: execution id wins over derived id.
	opts = pluginapi.SchedulerOptions{Metadata: map[string]any{
		"execution_session_id": "exec-1", "derived_session_id": "der-1"}}
	if got := extractSessionID(opts); got != "execution:exec-1" {
		t.Fatalf("execution got %q", got)
	}
	opts = pluginapi.SchedulerOptions{Metadata: map[string]any{"derived_session_id": "der-1"}}
	if got := extractSessionID(opts); got != "derived:der-1" {
		t.Fatalf("derived got %q", got)
	}
	if got := extractSessionID(pluginapi.SchedulerOptions{}); got != "" {
		t.Fatalf("empty got %q", got)
	}
}

func TestSchedulerSessionAffinity(t *testing.T) {
	now := time.Now()
	mk := func(id string) pluginapi.SchedulerAuthCandidate {
		return pluginapi.SchedulerAuthCandidate{ID: id, Attributes: map[string]string{"weight": "1"}}
	}
	a, b, banned := mk("openai-compatibility:ark-code:a"), mk("openai-compatibility:ark-code:b"), mk("openai-compatibility:ark-code:c")
	newP := func() *plugin {
		p := fixedPlugin(now)
		p.mu.Lock()
		p.arkAuths[a.ID], p.arkAuths[b.ID], p.arkAuths[banned.ID] = true, true, true
		p.strategy = strategyRoundRobin
		p.affinity = true
		p.affinityTTL = time.Hour
		p.mu.Unlock()
		p.bans.set(banned.ID, banEntry{ResetAt: now.Add(time.Hour)})
		return p
	}
	req := pluginapi.SchedulerPickRequest{
		Provider:   "ark-code",
		Model:      "glm-5.2",
		Candidates: []pluginapi.SchedulerAuthCandidate{a, b, banned},
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"derived_session_id": "sess-1"}},
	}

	// Same session sticks to the first picked key even though round-robin
	// would alternate.
	p := newP()
	ids := schedulerPickN(t, p, strategyRoundRobin, req, 4)
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("session not sticky: %v", ids)
		}
	}

	// A different session gets its own binding (round-robin cursor advanced
	// by the first session, so it lands on the other key).
	req2 := req
	req2.Options = pluginapi.SchedulerOptions{Metadata: map[string]any{"derived_session_id": "sess-2"}}
	ids2 := schedulerPickN(t, p, strategyRoundRobin, req2, 2)
	if ids2[0] == ids[0] {
		t.Fatalf("different session should rebind via strategy: %v vs %v", ids, ids2)
	}

	// Bound key gets banned -> affinity reselects from the remaining set.
	p2 := newP()
	first := schedulerPickN(t, p2, strategyRoundRobin, req, 1)[0]
	p2.bans.set(first, banEntry{ResetAt: now.Add(time.Hour)})
	next := schedulerPickN(t, p2, strategyRoundRobin, req, 1)[0]
	if next == first {
		t.Fatalf("bound key banned but still picked: %q", next)
	}

	// Expired binding is dropped and reselected.
	p3 := newP()
	past := now.Add(-2 * time.Hour)
	p3.now = func() time.Time { return past }
	schedulerPickN(t, p3, strategyRoundRobin, req, 1)
	p3.now = func() time.Time { return now } // TTL (1h) expired
	p3.affinityStore.set("ark-code::derived:sess-1::glm-5.2", banned.ID, time.Hour, past)
	got := schedulerPickN(t, p3, strategyRoundRobin, req, 1)[0]
	if got == banned.ID {
		t.Fatal("expired binding to banned key should not be honored")
	}
}

func TestAffinityTouchExtendsBinding(t *testing.T) {
	now := time.Now()
	mk := func(id string) pluginapi.SchedulerAuthCandidate {
		return pluginapi.SchedulerAuthCandidate{ID: id, Attributes: map[string]string{"weight": "1"}}
	}
	a, b, banned := mk("openai-compatibility:ark-code:a"), mk("openai-compatibility:ark-code:b"), mk("openai-compatibility:ark-code:c")
	p := fixedPlugin(now)
	p.mu.Lock()
	p.arkAuths[a.ID], p.arkAuths[b.ID], p.arkAuths[banned.ID] = true, true, true
	p.strategy = strategyRoundRobin
	p.affinity = true
	p.affinityTTL = time.Hour
	p.mu.Unlock()
	p.bans.set(banned.ID, banEntry{ResetAt: now.Add(24 * time.Hour)})
	req := pluginapi.SchedulerPickRequest{
		Provider:   "ark-code",
		Model:      "glm-5.2",
		Candidates: []pluginapi.SchedulerAuthCandidate{a, b, banned},
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"derived_session_id": "sess-1"}},
	}

	// Bind at t0, then keep hitting the binding every 30 minutes for 5 hours.
	// Without touch the binding would expire after the 1h TTL; with touch it
	// must stay alive and keep sticking to the same key.
	current := now
	p.now = func() time.Time { return current }
	first := schedulerPickN(t, p, strategyRoundRobin, req, 1)[0]
	for i := 0; i < 10; i++ {
		current = current.Add(30 * time.Minute)
		got := schedulerPickN(t, p, strategyRoundRobin, req, 1)[0]
		if got != first {
			t.Fatalf("binding lost at step %d: picked %q, want %q", i, got, first)
		}
	}
	// Idle past the TTL after the last touch: expires and reselects.
	current = current.Add(2 * time.Hour)
	p.bans.set(first, banEntry{ResetAt: current.Add(time.Hour)})
	got := schedulerPickN(t, p, strategyRoundRobin, req, 1)[0]
	if got == first {
		t.Fatal("idle binding should have expired and rebound away from banned key")
	}
}

func TestConfigArkPlanRecognized(t *testing.T) {
	p := newPlugin()
	p.readFile = func(path string) ([]byte, error) {
		if path == "/CLIProxyAPI/config.yaml" {
			return []byte(`openai-compatibility:
  - name: "ark-code"
    base-url: https://ark.cn-beijing.volces.com/api/coding/v3
    api-key-entries:
      - api-key: arksk-code-1 # code-one
    models:
      - name: "glm-5.3"
  - name: "ark-plan"
    base-url: https://ark.cn-beijing.volces.com/api/plan/v3
    api-key-entries:
      - api-key: arksk-plan-1 # plan-one
      - api-key: arksk-plan-2 # plan-two
    models:
      - name: "kimi-k3"
  - name: "openrouter"
    base-url: https://openrouter.ai/api/v1
    api-key-entries:
      - api-key: sk-or-1 # not-ark
routing:
  strategy: wrr
`), nil
		}
		return nil, os.ErrNotExist
	}
	configYAML := "config_path: /CLIProxyAPI/config.yaml\n"
	req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(configYAML)})
	p.configure(req)
	p.mu.RLock()
	defer p.mu.RUnlock()
	// 1 ark-code key + 2 ark-plan keys; openrouter skipped by base-url gate;
	// model "- name:" entries must not clobber provider tracking.
	if len(p.arkAuths) != 3 {
		t.Fatalf("arkAuths=%d, want 3 (%v)", len(p.arkAuths), p.arkAuths)
	}
	labels := map[string]bool{}
	for authID := range p.arkAuths {
		labels[p.keyLabels[authID]] = true
		if !strings.HasPrefix(authID, "openai-compatibility:ark-code:") && !strings.HasPrefix(authID, "openai-compatibility:ark-plan:") {
			t.Fatalf("unexpected authID %q", authID)
		}
	}
	for _, want := range []string{"code-one", "plan-one", "plan-two"} {
		if !labels[want] {
			t.Fatalf("missing label %q in %v", want, labels)
		}
	}
	if p.strategy != strategyWeightedRoundRobin {
		t.Fatalf("strategy=%q", p.strategy)
	}
}

func TestSchedulerNegativePriorityGrouping(t *testing.T) {
	now := time.Now()
	p := fixedPlugin(now)
	top1 := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:top1", Priority: -1}
	top2 := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:top2", Priority: -1}
	low := pluginapi.SchedulerAuthCandidate{ID: "openai-compatibility:ark-code:low", Priority: -5}
	p.mu.Lock()
	p.arkAuths[top1.ID], p.arkAuths[top2.ID], p.arkAuths[low.ID] = true, true, true
	p.mu.Unlock()
	// Ban one top-tier key so the plugin handles scheduling.
	p.bans.set(top2.ID, banEntry{ResetAt: now.Add(time.Hour)})
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider:   "ark-code",
		Model:      "glm-5.2",
		Candidates: []pluginapi.SchedulerAuthCandidate{low, top1, top2},
	})
	out, err := p.handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := envelopeResult[pluginapi.SchedulerPickResponse](t, out)
	// Only top1 survives: low must be excluded even though all priorities
	// are negative (0 must not act as a sentinel).
	if !got.Handled || got.AuthID != top1.ID {
		t.Fatalf("got=%+v, want %q", got, top1.ID)
	}
}

func TestRoutingConfigResetOnReload(t *testing.T) {
	p := newPlugin()
	p.readFile = func(path string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	// First load: wrr + affinity with custom TTL.
	p.readFile = func(path string) ([]byte, error) {
		if path == "/CLIProxyAPI/config.yaml" {
			return []byte("routing:\n  strategy: wrr\n  session-affinity: true\n  session-affinity-ttl: 2h\n"), nil
		}
		return nil, os.ErrNotExist
	}
	req, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("config_path: /CLIProxyAPI/config.yaml\n")})
	p.configure(req)
	p.mu.RLock()
	if p.strategy != strategyWeightedRoundRobin || !p.affinity || p.affinityTTL != 2*time.Hour {
		t.Fatalf("after wrr load: strategy=%q affinity=%v ttl=%v", p.strategy, p.affinity, p.affinityTTL)
	}
	p.mu.RUnlock()

	// Second load: routing keys removed -> must fall back to CPA defaults
	// (round-robin, affinity off, 1h TTL) instead of keeping stale values.
	p.readFile = func(path string) ([]byte, error) {
		if path == "/CLIProxyAPI/config.yaml" {
			return []byte("port: 8317\n"), nil
		}
		return nil, os.ErrNotExist
	}
	p.configure(req)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.strategy != strategyRoundRobin || p.affinity || p.affinityTTL != time.Hour {
		t.Fatalf("after reset: strategy=%q affinity=%v ttl=%v", p.strategy, p.affinity, p.affinityTTL)
	}
}
