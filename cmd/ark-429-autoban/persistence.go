package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// persistenceDir is determined at load time from the config_path parent dir.
// Falls back to /CLIProxyAPI/data/plugins/.
const bansFileName = "ark-429-autoban-bans.json"

// persistEntry is the JSON-serializable form of a ban entry.
type persistEntry struct {
	ResetAt   time.Time `json:"reset_at"`
	Window    string    `json:"window"`
	BannedAt  time.Time `json:"banned_at"`
	KeyHint   string    `json:"key_hint"`
	ErrorCode string    `json:"error_code,omitempty"`
}

// persistFile is the on-disk JSON structure.
type persistFile struct {
	Bans map[string]persistEntry `json:"bans"`
}

// startPersister runs a background goroutine that writes bans to disk
// when notified via the dirty channel. Writes are debounced by 1 second
// to coalesce rapid successive bans.
func (p *plugin) startPersister(dir string) {
	if dir == "" {
		return
	}
	go func() {
		var timer *time.Timer
		for {
			<-p.dirty
			// Debounce: wait 1s, coalesce multiple signals.
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(time.Second)
			<-timer.C
			// Drain any pending signals.
			for len(p.dirty) > 0 {
				<-p.dirty
			}
			p.saveBans(dir)
		}
	}()
}

func (p *plugin) markDirty() {
	select {
	case p.dirty <- struct{}{}:
	default: // channel full, a save is already pending
	}
}

// saveBans writes the current ban state to the JSON file.
func (p *plugin) saveBans(dir string) {
	snapshot := p.bans.snapshot()
	pf := persistFile{Bans: make(map[string]persistEntry, len(snapshot))}
	for authID, e := range snapshot {
		pf.Bans[authID] = persistEntry{
			ResetAt:   e.ResetAt,
			Window:    e.Window,
			BannedAt:  e.BannedAt,
			KeyHint:   e.KeyHint,
			ErrorCode: e.ErrorCode,
		}
	}
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		slog.Warn("ark-429-autoban: failed to marshal bans for persistence", "error", err)
		return
	}
	path := filepath.Join(dir, bansFileName)
	if err := os.WriteFile(path, data, 0600); err != nil {
		slog.Warn("ark-429-autoban: failed to write bans file", "path", path, "error", err)
		return
	}
	slog.Info("ark-429-autoban: persisted bans to file", "path", path, "count", len(pf.Bans))
}

// loadBans reads the JSON file and restores non-expired bans.
func (p *plugin) loadBans(dir string) {
	if dir == "" {
		return
	}
	path := filepath.Join(dir, bansFileName)
	data, err := p.readFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("ark-429-autoban: failed to read bans file", "path", path, "error", err)
		}
		return
	}
	var pf persistFile
	if err := json.Unmarshal(data, &pf); err != nil {
		slog.Warn("ark-429-autoban: failed to parse bans file", "path", path, "error", err)
		return
	}
	now := p.now()
	restored := 0
	for authID, e := range pf.Bans {
		if !now.Before(e.ResetAt) {
			continue // expired, skip
		}
		p.bans.set(authID, banEntry{
			ResetAt:   e.ResetAt,
			Window:    e.Window,
			BannedAt:  e.BannedAt,
			KeyHint:   e.KeyHint,
			ErrorCode: e.ErrorCode,
		})
		restored++
	}
	if restored > 0 {
		slog.Info("ark-429-autoban: restored bans from file", "path", path, "restored", restored, "skipped", len(pf.Bans)-restored)
	}
}

// resolvePersistDir determines the directory for the bans file.
// Uses the CPA plugins directory: parent of config_path + /data/plugins/.
func (p *plugin) resolvePersistDir() string {
	p.mu.RLock()
	cfgPath := p.configPath
	p.mu.RUnlock()
	if cfgPath != "" {
		return filepath.Join(filepath.Dir(cfgPath), "data", "plugins")
	}
	return "/CLIProxyAPI/data/plugins/"
}
