package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/m0rthis/dzman/internal/a2s"
	"github.com/m0rthis/dzman/internal/api"
	"github.com/m0rthis/dzman/internal/steam"
)

// serversLoadedMsg carries the result of a server-list fetch.
type serversLoadedMsg struct {
	list *api.ServerList
	err  error
}

// newsLoadedMsg carries the result of a news fetch.
type newsLoadedMsg struct {
	resp *api.NewsResponse
	err  error
}

// modsScannedMsg carries the result of a workshop scan.
type modsScannedMsg struct {
	mods []steam.InstalledMod
	err  error
}

// modsInstalledMsg carries the result of installing missing mods via steamcmd,
// including a fresh scan of both mod dirs.
type modsInstalledMsg struct {
	mods []steam.InstalledMod
	bad  []string // per-item reports for mods with a malformed ID
	err  error
}

// steamCMDUpdatedMsg is sent when Update Mods via steamcmd finishes.
type steamCMDUpdatedMsg struct {
	n   int
	bad []string // per-item reports for mods with a malformed ID
	err error
}

// steamLoginDoneMsg is sent when the interactive steamcmd login exits.
type steamLoginDoneMsg struct{ err error }

// countryResultMsg carries the geo-lookup result for a server IP.
type countryResultMsg struct {
	ip      string
	country string
}

// a2sResultMsg carries the live A2S_INFO query result for a server.
type a2sResultMsg struct {
	ip   string
	info *a2s.Info
	err  error
}

// battlemetricsResultMsg carries the Battlemetrics URL for the selected server.
type battlemetricsResultMsg struct {
	url string
}

// updateCheckMsg carries the latest GitHub release tag ("" = no releases).
type updateCheckMsg struct {
	latest string
	err    error
}

// errMsg wraps a generic background error.
type errMsg struct{ err error }

// gameLaunchedMsg is sent when LaunchDayZ returns (success or error).
type gameLaunchedMsg struct{ err error }

// gameStartedMsg is sent when the steam command for a listed server has
// started. wait delivers the gameLaunchedMsg.
type gameStartedMsg struct {
	joined *api.Server
	wait   tea.Cmd
}
