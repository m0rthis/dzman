package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// LaunchOption represents a single DayZ command-line launch option.
type LaunchOption struct {
	Enabled     bool    `json:"enabled"`
	Description string  `json:"description"`
	// Value is non-nil for options that take a parameter (e.g. -maxMem=4096).
	Value *string `json:"value,omitempty"`
}

// HistoryEntry is a server that was recently played.
type HistoryEntry struct {
	Name string    `json:"name"`
	IP   string    `json:"ip"`
	Port int       `json:"port"`
	At   time.Time `json:"ts"`
}

// FavoriteEntry is a bookmarked server.
type FavoriteEntry struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Profile holds all user-specific, persistent launcher data.
type Profile struct {
	SteamLogin string                  `json:"steam_login"`
	Player     string                  `json:"player"`
	SteamRoot  string                  `json:"steam_root"`
	Favorites  []FavoriteEntry         `json:"favorites"`
	History    []HistoryEntry          `json:"history"`
	Options    map[string]LaunchOption `json:"options"`
	Version    string                  `json:"version"`
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// DefaultLaunchOptions returns the built-in set of DayZ launch flags.
func DefaultLaunchOptions() map[string]LaunchOption {
	return map[string]LaunchOption{
		"window":              {Enabled: false, Description: "Launches in windowed mode"},
		"noborder":            {Enabled: false, Description: "Borderless windowed mode"},
		"nosplash":            {Enabled: true, Description: "Disables the splash screen on startup"},
		"skipintro":           {Enabled: true, Description: "Disables the intro video on startup"},
		"filePatching":        {Enabled: false, Description: "Enables the game to use unpacked local data"},
		"doLogs":              {Enabled: false, Description: "Force logging to file"},
		"buldozer":            {Enabled: false, Description: "Starts Buldozer (terrain editor) mode"},
		"high":                {Enabled: true, Description: "Give the process higher CPU priority"},
		"USEALLAVAILABLECORES": {Enabled: true, Description: "Use all available CPU cores"},
		"noPause":             {Enabled: false, Description: "Pause mode: -1 default, 0 graphics only, 1 graphics+sound"},
		"maxMem":              {Enabled: false, Description: "Maximum RAM in megabytes", Value: strPtr("")},
		"maxVRAM":             {Enabled: false, Description: "Maximum VRAM in megabytes", Value: strPtr("")},
		"cpuCount":            {Enabled: false, Description: "Number of CPUs/cores to use (2,4,6,8)", Value: strPtr("")},
		"exThreads":           {Enabled: false, Description: "Extra worker threads (0,1,3,5,7)", Value: strPtr("")},
		"noBenchmark":         {Enabled: false, Description: "Disable benchmarking"},
		"world":               {Enabled: true, Description: "Initial world to load (empty, ChernarusPlus)", Value: strPtr("empty")},
		"scriptDebug":         {Enabled: false, Description: "Enable script debugging"},
	}
}

// DefaultProfile returns an empty Profile with defaults filled in.
func DefaultProfile() *Profile {
	return &Profile{
		Favorites: []FavoriteEntry{},
		History:   []HistoryEntry{},
		Options:   DefaultLaunchOptions(),
		Version:   AppVersion,
	}
}

// LoadProfile reads the profile from path, returning defaults if not found.
func LoadProfile(path string) (*Profile, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return DefaultProfile(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile: %w", err)
	}

	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}

	// Back-fill any newly added default options.
	if p.Options == nil {
		p.Options = DefaultLaunchOptions()
	} else {
		for k, v := range DefaultLaunchOptions() {
			if _, ok := p.Options[k]; !ok {
				p.Options[k] = v
			}
		}
	}
	if p.Favorites == nil {
		p.Favorites = []FavoriteEntry{}
	}
	if p.History == nil {
		p.History = []HistoryEntry{}
	}

	return &p, nil
}

// Save writes the profile to path atomically (write to tmp then rename).
func (p *Profile) Save(path string) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write profile tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename profile: %w", err)
	}
	return nil
}

// LaunchArgs returns the list of enabled launch flags ready to pass to steam.
func (p *Profile) LaunchArgs() []string {
	// Fixed ordering so the arg string is deterministic.
	order := []string{
		"nosplash", "skipintro", "window", "noborder", "high",
		"USEALLAVAILABLECORES", "noBenchmark", "filePatching",
		"doLogs", "buldozer", "world", "noPause",
		"maxMem", "maxVRAM", "cpuCount", "exThreads", "scriptDebug",
	}
	var args []string
	for _, k := range order {
		opt, ok := p.Options[k]
		if !ok || !opt.Enabled {
			continue
		}
		if opt.Value != nil && *opt.Value != "" {
			args = append(args, fmt.Sprintf("-%s=%s", k, *opt.Value))
		} else {
			args = append(args, "-"+k)
		}
	}
	return args
}

// AddToHistory prepends an entry and trims to maxSize.
func (p *Profile) AddToHistory(name, ip string, port, maxSize int) {
	now := time.Now()
	trimmed := make([]HistoryEntry, 0, len(p.History))
	for _, h := range p.History {
		if h.IP != ip || h.Port != port {
			trimmed = append(trimmed, h)
		}
	}
	p.History = append([]HistoryEntry{{Name: name, IP: ip, Port: port, At: now}}, trimmed...)
	if len(p.History) > maxSize {
		p.History = p.History[:maxSize]
	}
}

// AddFavorite appends a server to the favorites list (no-op if already present).
func (p *Profile) AddFavorite(name, ip string, port int) {
	if p.IsFavorite(ip, port) {
		return
	}
	p.Favorites = append(p.Favorites, FavoriteEntry{Name: name, IP: ip, Port: port})
}

// RemoveFavorite removes the matching server from favorites.
func (p *Profile) RemoveFavorite(ip string, port int) {
	out := p.Favorites[:0]
	for _, f := range p.Favorites {
		if f.IP != ip || f.Port != port {
			out = append(out, f)
		}
	}
	p.Favorites = out
}

// IsFavorite reports whether ip:port is in the favorites list.
func (p *Profile) IsFavorite(ip string, port int) bool {
	for _, f := range p.Favorites {
		if f.IP == ip && f.Port == port {
			return true
		}
	}
	return false
}
