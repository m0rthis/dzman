package tui

import (
	"github.com/braydencw1/dzman/internal/a2s"
	"github.com/braydencw1/dzman/internal/api"
	"github.com/braydencw1/dzman/internal/steam"
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
// including a fresh workshop scan.
type modsInstalledMsg struct {
	mods []steam.InstalledMod
	err  error
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
