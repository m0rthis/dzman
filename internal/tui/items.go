package tui

import (
	"fmt"

	"github.com/braydencw1/dzman/internal/api"
	"github.com/braydencw1/dzman/internal/steam"
	"github.com/charmbracelet/bubbles/list"
)

// ---- serverItem — used in the server browser list ----

type serverItem struct {
	server api.Server
	idx    int // index in the master server slice
}

func (s serverItem) FilterValue() string {
	return s.server.Name + " " + s.server.Map
}

func (s serverItem) Title() string {
	players := fmt.Sprintf("%3d/%-3d", s.server.Players, s.server.MaxPlayers)
	lock := ""
	if s.server.Password {
		lock = "🔒"
	}
	env := envIcon(s.server.Environment)
	time := timeEmoji(s.server.Time)
	return fmt.Sprintf("%-52s %s  %s %s %s",
		truncate(s.server.Name, 52), players, time, env, lock,
	)
}

func (s serverItem) Description() string {
	mods := ""
	if n := len(s.server.Mods); n > 0 {
		mods = dimStyle.Render(fmt.Sprintf("[%d mods]", n))
	}
	be := boolIcon(s.server.BattlEye)
	return fmt.Sprintf("  %-20s  BE:%s  v%s  %s",
		truncate(s.server.Map, 20), be,
		truncate(s.server.Version, 12), mods,
	)
}

// ---- menuItem — used for simple text-label menus ----

type menuItem struct {
	label string
	desc  string
}

func (m menuItem) FilterValue() string { return m.label }
func (m menuItem) Title() string       { return m.label }
func (m menuItem) Description() string { return m.desc }

func newMenuItems(labels ...string) []list.Item {
	items := make([]list.Item, len(labels))
	for i, l := range labels {
		items[i] = menuItem{label: l}
	}
	return items
}

// ---- modItem — used in the installed mods view ----

type modItem struct {
	mod steam.InstalledMod
}

func (m modItem) FilterValue() string { return m.mod.Name }
func (m modItem) Title() string {
	managed := dimStyle.Render("steam")
	if m.mod.Managed {
		managed = greenStyle.Render("dzman")
	}
	return fmt.Sprintf("%-45s  %s  %s",
		truncate(m.mod.Name, 45),
		formatBytes(m.mod.Size),
		managed,
	)
}
func (m modItem) Description() string {
	desc := "ID: " + m.mod.ID
	if m.mod.MetaID != "" && m.mod.MetaID != m.mod.ID {
		desc += " (meta.cpp says " + m.mod.MetaID + ")"
	}
	return dimStyle.Render(desc)
}

// ---- favoriteItem / historyItem — wrappers for fav/history lists ----

type favoriteItem struct {
	name   string
	ip     string
	port   int
	online bool
	idx    int // index in server slice (-1 = offline)
}

func (f favoriteItem) FilterValue() string { return f.name + " " + f.ip }
func (f favoriteItem) Title() string {
	status := redStyle.Render("offline")
	if f.online {
		status = greenStyle.Render("online")
	}
	return fmt.Sprintf("%-50s  %s", truncate(f.name, 50), status)
}
func (f favoriteItem) Description() string {
	return dimStyle.Render(fmt.Sprintf("  %s:%d", f.ip, f.port))
}

type historyItem struct {
	favoriteItem
	timeAgo string
}

func (h historyItem) Title() string {
	status := redStyle.Render("offline")
	if h.online {
		status = greenStyle.Render("online")
	}
	return fmt.Sprintf("%-44s  %s  %s",
		truncate(h.name, 44), dimStyle.Render(h.timeAgo), status,
	)
}

// ---- filterItem — used in the filter multi-select menu ----

type filterItem struct {
	label  string
	active bool
}

func (f *filterItem) FilterValue() string { return f.label }
func (f *filterItem) Title() string {
	cb := checkboxOff
	if f.active {
		cb = checkboxOn
	}
	return cb + " " + f.label
}
func (f *filterItem) Description() string { return "" }

// ---- launchOptItem — for the launch-options toggle list ----

type launchOptItem struct {
	key     string
	enabled bool
	desc    string
	hasVal  bool
	val     string
}

func (l launchOptItem) FilterValue() string { return l.key }
func (l launchOptItem) Title() string {
	cb := checkboxOff
	if l.enabled {
		cb = checkboxOn
	}
	flag := blueStyle.Render("-" + l.key)
	val := ""
	if l.hasVal && l.val != "" {
		val = yellowStyle.Render("=" + l.val)
	}
	return fmt.Sprintf("%s %s%s", cb, flag, val)
}
func (l launchOptItem) Description() string { return dimStyle.Render("  " + l.desc) }
