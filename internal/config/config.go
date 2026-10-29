// Package config handles application configuration and user profile persistence.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/braydencw1/dzman/internal/version"
)

const (
	// AppName is the binary/data-directory name.
	AppName = "dzman"
	// GameID is the Steam App ID for DayZ.
	GameID = "221100"
)

// Mod backends for Config.ModBackend.
const (
	// BackendAuto uses Steamworks when libsteam_api.so is found and Steam is
	// running, else steamcmd when enabled and found, else manual.
	BackendAuto = "auto"
	// BackendSteamworks asks the running Steam client to download mods.
	BackendSteamworks = "steamworks"
	// BackendSteamCMD downloads mods with steamcmd into dzman's own dir.
	BackendSteamCMD = "steamcmd"
	// BackendManual opens the Workshop pages for the user to subscribe.
	BackendManual = "manual"
)

// Backends lists the ModBackend values in menu order.
var Backends = []string{BackendAuto, BackendSteamworks, BackendSteamCMD, BackendManual}

// Backend returns ModBackend, treating empty or unknown values as auto.
func (c *Config) Backend() string {
	for _, b := range Backends {
		if c.ModBackend == b {
			return b
		}
	}
	return BackendAuto
}

// AppVersion is the running application version, injected at build time via
// the version package (falls back to "dev" for plain builds).
var AppVersion = version.Version

// Duration wraps time.Duration so it serializes as a human-readable string
// ("5m", "1h") in the config file. Legacy nanosecond numbers still parse.
type Duration time.Duration

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON encodes the duration as a string like "5m0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string or a nanosecond number.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch val := v.(type) {
	case string:
		parsed, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", val, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(val))
	default:
		return fmt.Errorf("invalid duration value %v", v)
	}
	return nil
}

// Config holds the launcher's runtime configuration.
// Most fields have defaults; the struct is persisted to disk.
type Config struct {
	// APIBaseURL is the dayzsalauncher API base (default: https://dayzsalauncher.com/api/v1).
	APIBaseURL string `json:"api_base_url"`
	// RequestTimeout is the HTTP timeout for general requests.
	RequestTimeout Duration `json:"request_timeout"`
	// ServerDBTTL is how long the cached server list is valid.
	ServerDBTTL Duration `json:"server_db_ttl"`
	// NewsDBTTL is how long the cached news is valid.
	NewsDBTTL Duration `json:"news_db_ttl"`
	// ModBackend picks how missing mods are fetched: "auto", "steamworks",
	// "steamcmd" or "manual". See the Backend* constants.
	ModBackend string `json:"mod_backend"`
	// SteamAPIPath is an explicit libsteam_api.so for the Steamworks backend.
	// Empty means search ~/.config/dzman/ and the installed games.
	SteamAPIPath string `json:"steam_api_path"`
	// SteamworksTimeout is the overall limit for one Steamworks download run.
	SteamworksTimeout Duration `json:"steamworks_timeout"`
	// SteamworksStall aborts a Steamworks run after this long without progress.
	SteamworksStall Duration `json:"steamworks_stall"`
	// SteamCMDEnabled controls whether steamcmd may be used (the steamcmd
	// backend, and the auto backend's fallback).
	SteamCMDEnabled bool `json:"steamcmd_enabled"`
	// SteamCMDPath is the path to the steamcmd binary (default: "steamcmd").
	SteamCMDPath string `json:"steamcmd_path"`
	// FilterModLimit is the default threshold for the "mods < N" filter.
	FilterModLimit int `json:"filter_mod_limit"`
	// FilterPlayersLimit is the default threshold (%) for player-count filters.
	FilterPlayersLimit int `json:"filter_players_limit"`
	// FilterPlayersSlots is the minimum slot count for the slot filter.
	FilterPlayersSlots int `json:"filter_players_slots"`
	// HistorySize is the maximum number of history entries kept.
	HistorySize int `json:"history_size"`
	// Theme is the TUI accent colour: "purple", "blue", "orange" or a "#RRGGBB" hex.
	Theme string `json:"theme"`

	// Runtime paths — not persisted.
	Dir              string `json:"-"`
	ProfilePath      string `json:"-"`
	ConfPath         string `json:"-"`
	ServersCachePath string `json:"-"`
	NewsCachePath    string `json:"-"`
	UserAgent        string `json:"-"`
}

// defaults returns a Config populated with sensible defaults.
func defaults() *Config {
	return &Config{
		APIBaseURL:         "https://dayzsalauncher.com/api/v1",
		RequestTimeout:     Duration(10 * time.Second),
		ServerDBTTL:        Duration(5 * time.Minute),
		NewsDBTTL:          Duration(time.Hour),
		ModBackend:         BackendAuto,
		SteamworksTimeout:  Duration(30 * time.Minute),
		SteamworksStall:    Duration(5 * time.Minute),
		SteamCMDEnabled:    true,
		SteamCMDPath:       "steamcmd",
		FilterModLimit:     10,
		FilterPlayersLimit: 50,
		FilterPlayersSlots: 60,
		HistorySize:        10,
		Theme:              "blue",
	}
}

// Load returns the Config, reading from disk if it exists, or writing defaults.
func Load() (*Config, error) {
	dir, err := appDataDir()
	if err != nil {
		return nil, fmt.Errorf("locate data dir: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	cfg := defaults()
	cfg.setRuntimePaths(dir)

	if _, err := os.Stat(cfg.ConfPath); err == nil {
		data, readErr := os.ReadFile(cfg.ConfPath)
		if readErr != nil {
			return nil, fmt.Errorf("read config: %w", readErr)
		}
		if unmarshalErr := json.Unmarshal(data, cfg); unmarshalErr != nil {
			return nil, fmt.Errorf("parse config: %w", unmarshalErr)
		}
		// Re-populate runtime paths cleared by Unmarshal.
		cfg.setRuntimePaths(dir)
	}
	cfg.capLimits()

	return cfg, nil
}

// MaxSteamworksLimit caps steamworks_timeout and steamworks_stall. A run that
// long is already a stuck one, and the cap keeps deadline arithmetic from
// overflowing.
const MaxSteamworksLimit = Duration(24 * time.Hour)

// capLimits clamps the Steamworks limits to MaxSteamworksLimit.
func (c *Config) capLimits() {
	c.SteamworksTimeout = min(c.SteamworksTimeout, MaxSteamworksLimit)
	c.SteamworksStall = min(c.SteamworksStall, MaxSteamworksLimit)
}

func (c *Config) setRuntimePaths(dir string) {
	c.Dir = dir
	c.ProfilePath = filepath.Join(dir, "profile.json")
	c.ConfPath = filepath.Join(dir, AppName+".json")
	c.ServersCachePath = filepath.Join(dir, "servers.json")
	c.NewsCachePath = filepath.Join(dir, "news.json")
	c.UserAgent = AppName + " " + AppVersion
}

// Save writes the config to disk.
func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(c.ConfPath, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func appDataDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, AppName), nil
}
