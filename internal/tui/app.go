// Package tui provides the bubbletea-based terminal UI for dzman.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/m0rthis/dzman/internal/a2s"
	"github.com/m0rthis/dzman/internal/api"
	"github.com/m0rthis/dzman/internal/config"
	"github.com/m0rthis/dzman/internal/launcher"
	"github.com/m0rthis/dzman/internal/steam"
)

// page represents the currently active UI screen.
type page int

const (
	pageLoading        page = iota // fetching servers/news on startup
	pageSetup                      // first-run wizard
	pageMain                       // main menu
	pageBrowser                    // server list (all / filtered / favorites / history)
	pageServerAction               // post-select action (Play / Favorite / Desktop…)
	pageFilterMenu                 // multi-select filter list
	pageFilterInput                // text input for map/mod filter value
	pageDirectConnect              // IP + Port form
	pageNews                       // scrollable news view
	pageConfig                     // config submenu
	pageLaunchOpts                 // launch-options toggle list
	pageModInfo                    // installed mods list
	pageOfflineMission             // mission picker for offline play
	pageMissingMods                // server needs mods that are not installed
	pageModDownload                // Steamworks download progress
	pagePassword                   // password prompt for protected servers
	pageError                      // fatal / display error
)

// browserKind tracks what kind of data the browser is showing.
type browserKind int

const (
	browserAll browserKind = iota
	browserFiltered
	browserFavorites
	browserHistory
)

// setupStep tracks the first-run wizard step.
type setupStep int

const (
	setupWelcome      setupStep = iota // display welcome, ask SteamCMD
	setupLogin                         // ask steam login (if SteamCMD)
	setupLoginNow                      // offer an interactive SteamCMD login
	setupPlayer                        // ask player name
	setupSteamDir                      // ask steam root if not found
	setupSteamCMDPath                  // ask steamcmd binary path
	setupSteamAPIPath                  // ask libsteam_api.so path
)

// Model is the root bubbletea model for the entire application.
type Model struct {
	// Layout.
	w, h int

	// Current page.
	pg       page
	prevPage page

	// App state.
	cfg     *config.Config
	profile *config.Profile
	paths   *steam.Paths

	// Data.
	servers      []api.Server
	totalPlayers int
	news         []api.NewsArticle
	mods         []steam.InstalledMod

	// Async load tracking.
	loadedSrv  bool
	loadedNews bool
	loadedMods bool

	// Browser.
	bKind         browserKind
	activeFilters []filterDef
	displaySrvs   []api.Server // filtered/shown slice
	pendingFilter filterDef    // filter awaiting text input

	// Selected server (pageBrowser cursor → pageServerAction).
	sel        *api.Server
	selCountry string
	selPing    string
	selBMURL   string
	selLive    *a2s.Info // live A2S state for the selected server

	// countryCache avoids re-querying ip-api.com for IPs we already resolved.
	countryCache map[string]string

	// Pending launch state (missing-mods / password pages).
	missingMods []api.Mod
	missingNote string // the status line buildMissingModsNote showed
	// swFailed is set once a Steamworks attempt failed for missingMods, so
	// the page offers the steamcmd / manual fallback.
	swFailed bool
	// swallowC: c is ignored on the missing-mods page until then.
	swallowC time.Time

	// Mod backend, refreshed by refreshBackend.
	bk         backendInputs
	lib        libLookup // cached libsteam_api.so lookup
	libGen     int       // bumped to force a fresh lookup (see relookLibrary)
	backend    backendKind
	backendWhy string // why auto skipped Steamworks, "" if it did not
	// dl is the running or last Steamworks download.
	dl *dlState
	// resultToServer sends esc on the next text result page back to the
	// selected server's options (set by Update Server Mods, cleared on use).
	resultToServer bool
	pendingIP      string // direct-connect target not in the server list
	pendingPort    int

	// Setup wizard.
	step      setupStep
	useCMD    bool
	srvErr    error  // the server list failed to load
	mainNote  string // one-off status line on the main menu, cleared by the next key
	setupNote string // dim note shown on the player-name step (login result)

	// UI components.
	list     list.Model
	detail   viewport.Model
	newsView viewport.Model
	spin     spinner.Model

	// Text inputs (reused across pages: 0=primary, 1=secondary).
	inputs       [2]textinput.Model
	focusedInput int

	// Parallel bool slice for multi-select screens (filter / launch opts).
	multiSel []bool

	// Loading state.
	loading    bool
	loadingMsg string

	// Error state.
	err error
}

// New initialises a fresh Model ready for Init.
func New(cfg *config.Config, profile *config.Profile, paths *steam.Paths) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colAccent)

	ti0 := textinput.New()
	ti0.CharLimit = 256
	ti1 := textinput.New()
	ti1.CharLimit = 64

	return Model{
		cfg:          cfg,
		profile:      profile,
		paths:        paths,
		pg:           pageLoading,
		spin:         sp,
		inputs:       [2]textinput.Model{ti0, ti1},
		list:         newList(nil, "", 0, 0),
		countryCache: map[string]string{},
		loading:      true,
		loadingMsg:   "Fetching server list…",
		lib:          libLookup{pending: true}, // Init looks it up in the background
	}
}

// Init starts the spinner and fires the initial background loads.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.spin.Tick,
		m.fetchServersCmd(false),
		m.fetchNewsCmd(false),
		m.scanModsCmd(),
		m.findLibraryCmd(),
	)
}

// ---- Commands --------------------------------------------------------

// fetchServersCmd loads the server list from the disk cache when it is still
// within ServerDBTTL, otherwise from the API. force skips the cache.
func (m Model) fetchServersCmd(force bool) tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		sl, err := loadServers(cfg, force)
		if err != nil {
			return serversLoadedMsg{err: err}
		}
		return serversLoadedMsg{list: sl}
	}
}

// loadServers is the server-list lookup behind fetchServersCmd.
func loadServers(cfg *config.Config, force bool) (*api.ServerList, error) {
	if !force {
		if sl, ok := api.LoadCache[api.ServerList](cfg.ServersCachePath, cfg.ServerDBTTL.Std()); ok {
			return sl, nil
		}
	}
	return fetchServers(context.Background(), cfg)
}

// fetchServers downloads the server list and caches it.
func fetchServers(ctx context.Context, cfg *config.Config) (*api.ServerList, error) {
	sl, err := api.FetchServersContext(ctx, cfg.APIBaseURL, cfg.UserAgent)
	if err != nil {
		return nil, err
	}
	count, _ := api.FetchPlayersOnlineContext(ctx, cfg.UserAgent)
	sl.PlayersOnline = count
	_ = api.SaveCache(cfg.ServersCachePath, sl)
	return sl, nil
}

func (m Model) fetchNewsCmd(force bool) tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		if !force {
			if nr, ok := api.LoadCache[api.NewsResponse](cfg.NewsCachePath, cfg.NewsDBTTL.Std()); ok {
				return newsLoadedMsg{resp: nr}
			}
		}
		nr, err := api.FetchNews(cfg.UserAgent)
		if err == nil {
			_ = api.SaveCache(cfg.NewsCachePath, nr)
		}
		return newsLoadedMsg{resp: nr, err: err}
	}
}

func (m Model) scanModsCmd() tea.Cmd {
	paths := m.paths
	return func() tea.Msg {
		if paths == nil {
			return modsScannedMsg{}
		}
		mods, err := steam.ScanAllMods(paths)
		return modsScannedMsg{mods: mods, err: err}
	}
}

func updateModsCmd(paths *steam.Paths) tea.Cmd {
	return func() tea.Msg {
		if paths == nil {
			return modsScannedMsg{}
		}
		mods, err := steam.ScanAllMods(paths)
		return modsScannedMsg{mods: mods, err: err}
	}
}

func fetchCountryCmd(ip string) tea.Cmd {
	return func() tea.Msg {
		country, _ := api.FetchCountry(ip)
		return countryResultMsg{ip: ip, country: country}
	}
}

// fetchA2SCmd queries the server directly over UDP for live state and ping.
func fetchA2SCmd(ip string, queryPort int) tea.Cmd {
	return func() tea.Msg {
		info, err := a2s.Query(ip, queryPort, 3*time.Second)
		return a2sResultMsg{ip: ip, info: info, err: err}
	}
}

func fetchBMCmd(ip string, queryPort int, name, ua string) tea.Cmd {
	return func() tea.Msg {
		u, _ := api.BattlemetricsURL(ip, queryPort, name, ua)
		return battlemetricsResultMsg{url: u}
	}
}

func launchDayZCmd(playerName string, steamArgs []string) tea.Cmd {
	return func() tea.Msg {
		return gameLaunchedMsg{err: launcher.LaunchDayZ(playerName, steamArgs)}
	}
}

// launchServerCmd is launchDayZCmd for a listed server. It reports twice: a
// gameStartedMsg as soon as the steam command has started, so the model can
// record the join even if the user quits during the wait for the game, then
// the gameLaunchedMsg that wait command delivers. Neither touches the model
// from the command goroutine.
func launchServerCmd(srv *api.Server, playerName string, steamArgs []string) tea.Cmd {
	joined := *srv
	return func() tea.Msg {
		started := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- startDayZ(playerName, steamArgs, func() { started <- struct{}{} })
		}()
		startedMsg := func(wait tea.Cmd) tea.Msg { return gameStartedMsg{joined: &joined, wait: wait} }
		select {
		case <-started:
			return startedMsg(func() tea.Msg { return gameLaunchedMsg{err: <-done} })
		case err := <-done:
			// It may have finished right after calling started; both are
			// ready then and select picks either.
			select {
			case <-started:
				return startedMsg(func() tea.Msg { return gameLaunchedMsg{err: err} })
			default:
				return gameLaunchedMsg{err: err}
			}
		}
	}
}

// steamCMDUpdateCmd re-downloads the dzman-managed steamcmd mods into
// contentDir. Mods with a malformed ID are skipped and reported. The game
// itself is left to the Steam client.
func steamCMDUpdateCmd(steamcmdPath, login, contentDir string, mods []steam.InstalledMod) tea.Cmd {
	return func() tea.Msg {
		var bad []string
		n := 0
		for _, mod := range mods {
			if !steam.ValidModID(mod.ID) {
				bad = append(bad, fmt.Sprintf("%s: invalid workshop ID %q", mod.Name, mod.ID))
				continue
			}
			if err := steam.DownloadMod(steamcmdPath, login, contentDir, mod.ID); err != nil {
				return steamCMDUpdatedMsg{n: n, bad: bad, err: loginHint(err)}
			}
			n++
		}
		return steamCMDUpdatedMsg{n: n, bad: bad}
	}
}

// loginHint tells the user how to log in when steamcmd has no credentials.
func loginHint(err error) error {
	if errors.Is(err, steam.ErrLoginRequired) {
		return fmt.Errorf("SteamCMD is not logged in; press l on a server's missing-mods page, "+
			"or set the login under Config > Change Steam Login: %w", err)
	}
	return err
}

// steamLoginCmd suspends the TUI and runs an interactive steamcmd login so
// the user can type the password / Steam Guard code.
func (m Model) steamLoginCmd() tea.Cmd {
	path, err := m.resolveSteamCMD()
	if err != nil {
		return func() tea.Msg { return steamLoginDoneMsg{err: err} }
	}
	cmd, err := steam.LoginCmd(path, m.profile.SteamLogin)
	if err != nil {
		return func() tea.Msg { return steamLoginDoneMsg{err: err} }
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return steamLoginDoneMsg{err: err}
	})
}

// installMissingModsCmd downloads mods via steamcmd into dzman's own dir
// (DownloadMod marks them managed) and returns a fresh scan.
// Mods with a malformed ID are not passed to steamcmd; they are reported.
func installMissingModsCmd(steamcmdPath, login string, paths *steam.Paths, mods []api.Mod) tea.Cmd {
	return func() tea.Msg {
		bad, err := installMissingMods(context.Background(), steamcmdPath, login, paths, mods)
		if err != nil {
			return modsInstalledMsg{bad: bad, err: err}
		}
		installed, err := steam.ScanAllMods(paths)
		return modsInstalledMsg{mods: installed, bad: bad, err: err}
	}
}

// installMissingMods downloads mods with steamcmd, one at a time. It returns
// the per-item reports for mods with a malformed ID, which are skipped.
// ctx is checked between mods, so a cancelled join stops before the next one.
func installMissingMods(ctx context.Context, steamcmdPath, login string, paths *steam.Paths, mods []api.Mod) (bad []string, err error) {
	seen := map[string]bool{}
	for _, mod := range mods {
		if err := ctx.Err(); err != nil {
			return bad, err
		}
		if seen[mod.SteamWorkshopID] {
			continue // listed twice; one download is enough
		}
		seen[mod.SteamWorkshopID] = true
		if !steam.ValidModID(mod.SteamWorkshopID) {
			bad = append(bad, invalidModLine(mod))
			continue
		}
		if err := steam.DownloadModContext(ctx, steamcmdPath, login, paths.CmdContent, mod.SteamWorkshopID); err != nil {
			return bad, err
		}
	}
	return bad, nil
}

// invalidModLine is the per-item report for a server mod with a bad ID.
func invalidModLine(mod api.Mod) string {
	return fmt.Sprintf("%s: invalid workshop ID %q", mod.Name, mod.SteamWorkshopID)
}

// openWorkshopCmd opens each mod's Steam Workshop page in the browser.
func openWorkshopCmd(mods []api.Mod) tea.Cmd {
	return func() tea.Msg {
		for _, mod := range mods {
			_ = exec.Command("xdg-open", mod.WorkshopURL()).Start()
		}
		return nil
	}
}

// checkUpdateCmd fetches the latest release tag from GitHub.
func checkUpdateCmd(userAgent string) tea.Cmd {
	return func() tea.Msg {
		latest, err := api.LatestRelease(userAgent)
		return updateCheckMsg{latest: latest, err: err}
	}
}

// ---- Update ----------------------------------------------------------

// Update dispatches all messages to the current page handler.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resizeComponents()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case serversLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.srvErr = msg.err
		} else {
			m.servers = msg.list.Result
			m.totalPlayers = msg.list.PlayersOnline
		}
		m.loadedSrv = true
		return m.maybeFinishLoading()

	case newsLoadedMsg:
		if msg.resp != nil {
			m.news = msg.resp.Rows
		}
		m.loadedNews = true
		return m.maybeFinishLoading()

	case modsScannedMsg:
		m.mods = msg.mods
		if m.pg == pageConfig {
			return m.rebuildConfigMenu() // e.g. after Remove Managed Mods
		}
		if m.pg != pageLoading || m.loadedMods {
			// A background rescan. A pageLoading that is not waiting on the
			// mod scan (installing, launching…) is not ours to end.
			return m, nil
		}
		m.loadedMods = true
		return m.maybeFinishLoading()

	case libFoundMsg:
		if msg.key != m.libKey() {
			if m.lib.pending {
				// The inputs changed while the startup lookup ran.
				return m, m.findLibraryCmd()
			}
			return m, nil
		}
		m.lib = libLookup(msg)
		m.lib.pending = false
		cmd := m.refreshBackend()
		switch m.pg {
		case pageConfig:
			next, cmd2 := m.rebuildConfigMenu()
			return next, tea.Batch(cmd, cmd2)
		case pageMissingMods:
			// The backend may have been undecided until now.
			next, cmd2 := m.buildMissingModsNote(m.missingMods, m.missingNote)
			return next, tea.Batch(cmd, cmd2)
		}
		return m, cmd

	case countryResultMsg:
		m.countryCache[msg.ip] = msg.country
		if m.sel != nil && m.sel.Endpoint.IP == msg.ip {
			m.selCountry = msg.country
			m.refreshDetail()
		}
		return m, nil

	case a2sResultMsg:
		if m.sel != nil && m.sel.Endpoint.IP == msg.ip {
			if msg.err != nil {
				m.selPing = redStyle.Render("timeout")
				m.selLive = nil
			} else {
				m.selPing = fmt.Sprintf("%dms", msg.info.Ping.Milliseconds())
				m.selLive = msg.info
			}
			m.refreshDetail()
		}
		return m, nil

	case modsInstalledMsg:
		m.loading = false
		if errors.Is(msg.err, steam.ErrLoginRequired) && len(m.missingMods) > 0 {
			return m.buildMissingModsNote(m.missingMods, redStyle.Render(
				"SteamCMD needs a login. Press l to log in, then i to install."))
		}
		if msg.err != nil {
			m.err = msg.err
			m.pg = pageError
			return m, nil
		}
		m.mods = msg.mods
		if m.sel == nil {
			m.pg = pageMain
			return m.buildMainList()
		}
		if missing := m.missingServerMods(m.sel); len(missing) > 0 {
			note := fmt.Sprintf("%d mods still missing after install.", len(missing))
			if len(msg.bad) < len(missing) {
				note += " SteamCMD may need a login: press l."
			}
			if len(msg.bad) > 0 {
				note += "\n" + strings.Join(msg.bad, "\n")
			}
			return m.buildMissingModsNote(missing, redStyle.Render(note))
		}
		return m.promptPasswordOrLaunch(m.sel)

	case steamLoginDoneMsg:
		return m.finishSteamLogin(msg)

	case swStartedMsg, swEventMsg, swGoneMsg, swExitMsg, swRescanMsg:
		return m.updateSteamworks(msg)

	case steamCMDUpdatedMsg:
		body := greenStyle.Render(fmt.Sprintf("  Updated %d SteamCMD mods.", msg.n))
		if msg.err != nil {
			body = redStyle.Render("  " + msg.err.Error())
		}
		for _, b := range msg.bad {
			body += "\n" + redStyle.Render("  "+b)
		}
		m, _ = m.showModResult("Update Mods", body)
		return m, m.scanModsCmd()

	case updateCheckMsg:
		return m.showUpdateResult(msg)

	case battlemetricsResultMsg:
		m.selBMURL = msg.url
		m.refreshDetail()
		return m, nil

	case gameLaunchedMsg:
		if msg.err != nil {
			m.err = msg.err
			m.pg = pageError
			return m, nil
		}
		return m, tea.Quit

	case gameStartedMsg:
		recordJoin(m.cfg, m.profile, msg.joined)
		return m, msg.wait

	case errMsg:
		m.err = msg.err
		m.pg = pageError
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m.updateComponent(msg)
}

func (m Model) maybeFinishLoading() (Model, tea.Cmd) {
	if !m.loadedSrv || !m.loadedNews || !m.loadedMods {
		return m, nil
	}
	m.loading = false

	if m.err != nil {
		m.pg = pageError
		return m, nil
	}

	if m.profile.Player == "" {
		m.pg = pageSetup
		m.step = setupWelcome
		items := newMenuItems(
			"Yes — also allow SteamCMD (needs its own Steam login)",
			"No — use the running Steam client, or subscribe manually",
		)
		m.list = newList(items, "dzman installs mods through your running Steam client. Allow SteamCMD as a fallback?", m.w, m.h-6)
		return m, nil
	}

	m.pg = pageMain
	return m.buildMainList()
}

func (m Model) updateComponent(msg tea.Msg) (Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.pg {
	case pageBrowser:
		m.list, cmd = m.list.Update(msg)
		newSel := m.browserSelected()
		if newSel != nil && newSel != m.sel {
			m.sel = newSel
			return m, tea.Batch(cmd, m.onSelChanged())
		}
		return m, cmd

	case pageMain, pageServerAction, pageFilterMenu,
		pageConfig, pageLaunchOpts, pageModInfo, pageOfflineMission:
		m.list, cmd = m.list.Update(msg)

	case pageNews, pageMissingMods:
		m.newsView, cmd = m.newsView.Update(msg)

	case pageDirectConnect, pageFilterInput:
		m.inputs[m.focusedInput], cmd = m.inputs[m.focusedInput].Update(msg)

	case pagePassword:
		m.inputs[0], cmd = m.inputs[0].Update(msg)

	case pageSetup:
		if m.step == setupWelcome || m.step == setupLoginNow {
			m.list, cmd = m.list.Update(msg)
		} else {
			m.inputs[0], cmd = m.inputs[0].Update(msg)
		}
	}
	return m, cmd
}

// ---- Key handling ----------------------------------------------------

func (m Model) handleKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch m.pg {
	case pageLoading:
		return m.keyLoading(msg)
	case pageSetup:
		return m.keySetup(msg)
	case pageMain:
		return m.keyMain(msg)
	case pageBrowser:
		return m.keyBrowser(msg)
	case pageServerAction:
		return m.keyServerAction(msg)
	case pageFilterMenu:
		return m.keyFilterMenu(msg)
	case pageFilterInput:
		return m.keyFilterInput(msg)
	case pageDirectConnect:
		return m.keyDirectConnect(msg)
	case pageNews:
		return m.keyNews(msg)
	case pageConfig:
		return m.keyConfig(msg)
	case pageLaunchOpts:
		return m.keyLaunchOpts(msg)
	case pageModInfo:
		return m.keyModInfo(msg)
	case pageOfflineMission:
		return m.keyOfflineMission(msg)
	case pageMissingMods:
		return m.keyMissingMods(msg)
	case pageModDownload:
		return m.keyModDownload(msg)
	case pagePassword:
		return m.keyPassword(msg)
	case pageError:
		return m.keyError(msg)
	}
	return m, nil
}

func (m Model) keyLoading(msg tea.KeyMsg) (Model, tea.Cmd) {
	if msg.String() == "q" || msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) keyError(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "enter", "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

// ---- Setup wizard ----------------------------------------------------

func (m Model) keySetup(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch m.step {
	case setupWelcome:
		return m.keySetupWelcome(msg)
	case setupLogin:
		return m.keySetupInput(msg, m.advanceSetupLogin)
	case setupLoginNow:
		return m.keySetupLoginNow(msg)
	case setupPlayer:
		return m.keySetupInput(msg, m.advanceSetupPlayer)
	case setupSteamDir:
		return m.keySetupInput(msg, m.advanceSetupSteamDir)
	case setupSteamCMDPath:
		return m.keySetupInput(msg, m.advanceSetupSteamCMDPath)
	case setupSteamAPIPath:
		return m.keySetupInput(msg, m.advanceSetupSteamAPIPath)
	}
	return m, nil
}

func (m Model) keySetupWelcome(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		i, ok := m.list.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		m.useCMD = strings.Contains(i.label, "Yes")
		m.cfg.SteamCMDEnabled = m.useCMD
		if err := m.cfg.Save(); err != nil {
			m.err = err
			m.pg = pageError
			return m, nil
		}
		m.step = setupLogin
		if !m.useCMD {
			m.step = setupPlayer
		}
		m.inputs[0] = textinput.New()
		if m.step == setupLogin {
			m.inputs[0].Placeholder = "anonymous"
			m.inputs[0].Prompt = "Steam login: "
		} else {
			m.inputs[0].Placeholder = "Survivor"
			m.inputs[0].Prompt = "Player name: "
		}
		m.inputs[0].Focus()
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m Model) keySetupInput(msg tea.KeyMsg, advance func() (Model, tea.Cmd)) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return advance()
	case "esc":
		// The path prompts are only reached from Config; leave unsaved.
		if m.step == setupSteamCMDPath || m.step == setupSteamAPIPath {
			return m.buildConfigMenu()
		}
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.inputs[0], cmd = m.inputs[0].Update(msg)
	return m, cmd
}

func (m Model) advanceSetupLogin() (Model, tea.Cmd) {
	login := strings.TrimSpace(m.inputs[0].Value())
	if login == "" {
		login = "anonymous"
	}
	if err := steam.ValidateSteamLogin(login); err != nil {
		m.setupNote = err.Error()
		return m, nil
	}
	m.setupNote = ""
	m.profile.SteamLogin = login
	if m.cfg.SteamCMDEnabled && login != "anonymous" {
		m.step = setupLoginNow
		items := newMenuItems(
			"Yes — log in to SteamCMD now (password / Steam Guard)",
			"No — skip",
		)
		m.list = newList(items, "Log in to SteamCMD now?", m.w, m.h-6)
		return m, nil
	}
	return m.toPlayerStep(), nil
}

// toPlayerStep moves the wizard to the player-name prompt.
func (m Model) toPlayerStep() Model {
	m.step = setupPlayer
	m.inputs[0] = textinput.New()
	m.inputs[0].Placeholder = "Survivor"
	m.inputs[0].Prompt = "Player name: "
	m.inputs[0].Focus()
	return m
}

func (m Model) keySetupLoginNow(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		i, ok := m.list.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		if strings.Contains(i.label, "Yes") {
			return m, m.steamLoginCmd()
		}
		return m.toPlayerStep(), nil
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// finishSteamLogin handles the end of an interactive steamcmd login: back to
// the missing-mods page, or on with the setup wizard.
func (m Model) finishSteamLogin(msg steamLoginDoneMsg) (Model, tea.Cmd) {
	if m.pg == pageSetup {
		m = m.toPlayerStep()
		if msg.err != nil {
			m.setupNote = "SteamCMD login did not finish: " + msg.err.Error()
		}
		return m, nil
	}
	if msg.err != nil {
		return m.buildMissingModsNote(m.missingMods, redStyle.Render("SteamCMD login failed: "+msg.err.Error()))
	}
	return m.buildMissingModsNote(m.missingMods, greenStyle.Render("SteamCMD login finished. Press i to install."))
}

func (m Model) advanceSetupPlayer() (Model, tea.Cmd) {
	name := strings.TrimSpace(m.inputs[0].Value())
	if name == "" {
		name = "Survivor"
	}
	m.profile.Player = name
	m.setupNote = ""
	if m.paths == nil {
		m.step = setupSteamDir
		m.inputs[0] = textinput.New()
		m.inputs[0].Placeholder = "~/.steam/steam"
		m.inputs[0].Prompt = "Steam root: "
		m.inputs[0].Focus()
		return m, nil
	}
	return m.finishSetup()
}

func (m Model) advanceSetupSteamCMDPath() (Model, tea.Cmd) {
	path := strings.TrimSpace(m.inputs[0].Value())
	if path == "" {
		path = steam.DefaultSteamCMD
	}
	m.cfg.SteamCMDPath = path
	_ = m.cfg.Save()
	return m.buildConfigMenu()
}

func (m Model) advanceSetupSteamAPIPath() (Model, tea.Cmd) {
	m.cfg.SteamAPIPath = strings.TrimSpace(m.inputs[0].Value())
	_ = m.cfg.Save()
	// Look again even if the value is unchanged; buildConfigMenu starts
	// the lookup in the background.
	m.relookLibrary()
	return m.buildConfigMenu()
}

func (m Model) advanceSetupSteamDir() (Model, tea.Cmd) {
	dir := strings.TrimSpace(m.inputs[0].Value())
	p, err := steam.DetectPaths(dir)
	if err != nil {
		m.err = fmt.Errorf("path not found: %w", err)
		m.pg = pageError
		return m, nil
	}
	m.paths = p
	m.profile.SteamRoot = dir
	return m.finishSetup()
}

func (m Model) finishSetup() (Model, tea.Cmd) {
	if err := m.profile.Save(m.cfg.ProfilePath); err != nil {
		m.err = err
		m.pg = pageError
		return m, nil
	}
	m.pg = pageMain
	return m.buildMainList()
}

// ---- Main menu -------------------------------------------------------

func (m Model) buildMainList() (Model, tea.Cmd) {
	var labels []string
	if len(m.profile.History) > 0 {
		labels = append(labels, rejoinLabel(m.profile))
	}
	labels = append(labels, "All Servers", "Filter Servers")
	if len(m.profile.Favorites) > 0 {
		labels = append(labels, "Favorites")
	}
	if len(m.profile.History) > 0 {
		labels = append(labels, "Recently Played")
	}
	labels = append(labels,
		"Direct Connect", "Launch Game", "Play Offline",
		"DayZ News", "Installed Mods", "Config",
	)
	m.list = newList(newMenuItems(labels...), "DayZ Manager", m.w, m.h-8)
	if len(m.profile.History) > 0 {
		m.list.Select(1) // not the Rejoin item: Enter should not join a server by accident
	}
	return m, nil
}

func (m Model) keyMain(msg tea.KeyMsg) (Model, tea.Cmd) {
	m.mainNote = ""
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "enter":
		i, ok := m.list.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		return m.handleMainSelect(i.label)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// rejoinLabel is the main-menu item for the most recent server in the history.
func rejoinLabel(p *config.Profile) string { return "Rejoin " + SanitizeText(p.History[0].Name) }

// rejoin joins the most recent server in the history the way picking it under
// Recently Played does: through the server's mod check, password and launch.
func (m Model) rejoin() (Model, tea.Cmd) {
	t, err := RejoinTarget(m.profile)
	if err == nil {
		if srv := findServer(m.servers, t); srv != nil {
			m.prevPage = pageMain
			m.sel = srv
			return m.playServer(srv)
		}
		switch {
		case !m.loadedSrv:
			m.mainNote = "The server list is still loading"
		case m.srvErr != nil:
			m.mainNote = "The server list failed to load: " + SanitizeText(m.srvErr.Error())
		default:
			m.mainNote = SanitizeText(t.Name) + " isn't in the server list right now"
		}
		return m, nil
	}
	m.mainNote = SanitizeText(err.Error())
	return m, nil
}

func (m Model) handleMainSelect(label string) (Model, tea.Cmd) {
	if len(m.profile.History) > 0 && label == rejoinLabel(m.profile) {
		return m.rejoin()
	}
	switch label {
	case "All Servers":
		m.bKind = browserAll
		m.activeFilters = nil
		m.displaySrvs = m.servers
		m.pg = pageBrowser
		return m.buildBrowser()
	case "Filter Servers":
		return m.buildFilterMenu()
	case "Favorites":
		m.bKind = browserFavorites
		m.pg = pageBrowser
		return m.buildFavoritesBrowser()
	case "Recently Played":
		m.bKind = browserHistory
		m.pg = pageBrowser
		return m.buildHistoryBrowser()
	case "Direct Connect":
		return m.buildDirectConnect()
	case "Launch Game":
		return m.launchGame()
	case "Play Offline":
		return m.buildOfflineMission()
	case "DayZ News":
		return m.buildNews()
	case "Installed Mods":
		return m.buildModInfo()
	case "Config":
		return m.buildConfigMenu()
	}
	return m, nil
}

// ---- Browser ---------------------------------------------------------

func (m Model) buildBrowser() (Model, tea.Cmd) {
	items := make([]list.Item, len(m.displaySrvs))
	for i := range m.displaySrvs {
		s := m.displaySrvs[i]
		items[i] = serverItem{server: s, idx: i}
	}
	title := "All Servers"
	switch m.bKind {
	case browserFiltered:
		title = fmt.Sprintf("Filtered Servers (%d)", len(m.displaySrvs))
	case browserFavorites:
		title = "Favorites"
	case browserHistory:
		title = "Recently Played"
	}

	listW := m.w
	if m.w >= 120 {
		listW = m.w / 2
	}
	m.list = newList(items, title, listW, m.h-4)

	vpW := m.w - listW - 4
	if vpW > 10 {
		m.detail = viewport.New(vpW, m.h-4)
	}

	if len(m.displaySrvs) > 0 {
		m.sel = &m.displaySrvs[0]
		return m, m.onSelChanged()
	}
	return m, nil
}

// onSelChanged resets the detail pane for the newly selected server and
// returns the async lookups still needed (country is cached per IP).
func (m *Model) onSelChanged() tea.Cmd {
	m.selPing, m.selBMURL = "…", ""
	m.selLive = nil
	cmds := []tea.Cmd{
		fetchA2SCmd(m.sel.Endpoint.IP, m.sel.Endpoint.Port),
		fetchBMCmd(m.sel.Endpoint.IP, m.sel.Endpoint.Port, m.sel.Name, m.cfg.UserAgent),
	}
	if country, ok := m.countryCache[m.sel.Endpoint.IP]; ok {
		m.selCountry = country
	} else {
		m.selCountry = "…"
		cmds = append(cmds, fetchCountryCmd(m.sel.Endpoint.IP))
	}
	m.refreshDetail()
	return tea.Batch(cmds...)
}

func (m Model) buildFavoritesBrowser() (Model, tea.Cmd) {
	items := make([]list.Item, 0, len(m.profile.Favorites))
	for _, fav := range m.profile.Favorites {
		fi := favoriteItem{name: fav.Name, ip: fav.IP, port: fav.Port, idx: -1}
		for i := range m.servers {
			s := &m.servers[i]
			if s.Endpoint.IP == fav.IP && s.Endpoint.Port == fav.Port {
				fi.online = true
				fi.idx = i
				break
			}
		}
		items = append(items, fi)
	}
	m.list = newList(items, "Favorites", m.w, m.h-4)
	m.displaySrvs = nil
	return m, nil
}

func (m Model) buildHistoryBrowser() (Model, tea.Cmd) {
	now := time.Now()
	items := make([]list.Item, len(m.profile.History))
	for i, h := range m.profile.History {
		fi := favoriteItem{name: h.Name, ip: h.IP, port: h.Port, idx: -1}
		for j := range m.servers {
			s := &m.servers[j]
			if s.Endpoint.IP == h.IP && s.Endpoint.Port == h.Port {
				fi.online = true
				fi.idx = j
				break
			}
		}
		items[i] = historyItem{favoriteItem: fi, timeAgo: humanDuration(now.Sub(h.At))}
	}
	m.list = newList(items, "Recently Played", m.w, m.h-4)
	m.displaySrvs = nil
	return m, nil
}

func (m Model) keyBrowser(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageMain
		return m.buildMainList()
	case "enter":
		return m.selectFromBrowser()
	case "f":
		if m.sel != nil {
			if m.profile.IsFavorite(m.sel.Endpoint.IP, m.sel.Endpoint.Port) {
				m.profile.RemoveFavorite(m.sel.Endpoint.IP, m.sel.Endpoint.Port)
			} else {
				m.profile.AddFavorite(m.sel.Name, m.sel.Endpoint.IP, m.sel.Endpoint.Port)
			}
			_ = m.profile.Save(m.cfg.ProfilePath)
		}
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	newSel := m.browserSelected()
	if newSel != nil && newSel != m.sel {
		m.sel = newSel
		return m, tea.Batch(cmd, m.onSelChanged())
	}
	return m, cmd
}

// browserSelected returns the api.Server for the currently highlighted item.
func (m Model) browserSelected() *api.Server {
	item := m.list.SelectedItem()
	if item == nil {
		return nil
	}
	switch v := item.(type) {
	case serverItem:
		if v.idx < len(m.displaySrvs) {
			return &m.displaySrvs[v.idx]
		}
	case favoriteItem:
		if v.idx >= 0 && v.idx < len(m.servers) {
			return &m.servers[v.idx]
		}
	case historyItem:
		if v.idx >= 0 && v.idx < len(m.servers) {
			return &m.servers[v.idx]
		}
	}
	return nil
}

func (m Model) selectFromBrowser() (Model, tea.Cmd) {
	item := m.list.SelectedItem()
	if item == nil {
		return m, nil
	}
	m.prevPage = pageBrowser

	var srv *api.Server
	var offName, offIP string
	var offPort int

	switch v := item.(type) {
	case serverItem:
		if v.idx < len(m.displaySrvs) {
			srv = &m.displaySrvs[v.idx]
		}
	case favoriteItem:
		if v.idx >= 0 {
			srv = &m.servers[v.idx]
		} else {
			offName, offIP, offPort = v.name, v.ip, v.port
		}
	case historyItem:
		if v.idx >= 0 {
			srv = &m.servers[v.idx]
		} else {
			offName, offIP, offPort = v.name, v.ip, v.port
		}
	}

	m.sel = srv
	m.pg = pageServerAction
	return m.buildServerAction(srv, offName, offIP, offPort)
}

// ---- Filter menu -----------------------------------------------------

func (m Model) buildFilterMenu() (Model, tea.Cmd) {
	opts := allFilterOptions()
	items := make([]list.Item, len(opts))
	m.multiSel = make([]bool, len(opts))

	for i, o := range opts {
		active := false
		for _, af := range m.activeFilters {
			if af.kind == o.kind {
				active = true
				break
			}
		}
		m.multiSel[i] = active
		items[i] = &filterItem{label: o.label, active: active}
	}
	m.list = newList(items, "Filters  [space] toggle  [enter] apply", m.w, m.h-4)
	m.list.SetFilteringEnabled(false)
	m.pg = pageFilterMenu
	return m, nil
}

func (m Model) keyFilterMenu(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	case " ":
		idx := m.list.Index()
		if idx < 0 || idx >= len(m.multiSel) {
			return m, nil
		}
		m.multiSel[idx] = !m.multiSel[idx]
		opts := allFilterOptions()
		if idx < len(opts) {
			return m, m.list.SetItem(idx, &filterItem{
				label:  opts[idx].label,
				active: m.multiSel[idx],
			})
		}
		return m, nil
	case "enter":
		return m.applyFilterSelection()
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m Model) applyFilterSelection() (Model, tea.Cmd) {
	opts := allFilterOptions()
	m.activeFilters = nil

	for i, active := range m.multiSel {
		if !active || i >= len(opts) {
			continue
		}
		fd := opts[i]
		needsInput := fd.kind == filterByMap ||
			fd.kind == filterByModName ||
			fd.kind == filterByModID ||
			fd.kind == filterModsLT ||
			fd.kind == filterModsGT ||
			fd.kind == filterPlayersAbovePct ||
			fd.kind == filterPlayersBelowPct ||
			fd.kind == filterMinSlots

		if needsInput {
			m.pendingFilter = fd
			m.pg = pageFilterInput
			m.inputs[0] = textinput.New()
			m.inputs[0].Placeholder = filterPrompt(fd.kind, m.cfg)
			m.inputs[0].Prompt = fd.label + ": "
			m.inputs[0].Focus()
			return m, nil
		}
		m.activeFilters = append(m.activeFilters, fd)
	}

	return m.openFilteredBrowser()
}

func (m Model) keyFilterInput(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		val := strings.TrimSpace(m.inputs[0].Value())
		if val == "" {
			val = m.inputs[0].Placeholder
		}
		m.pendingFilter.value = val
		m.activeFilters = append(m.activeFilters, m.pendingFilter)
		return m.applyFilterSelection()
	case "esc":
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.inputs[0], cmd = m.inputs[0].Update(msg)
	return m, cmd
}

func (m Model) openFilteredBrowser() (Model, tea.Cmd) {
	m.bKind = browserFiltered
	m.displaySrvs = applyFilters(m.servers, m.activeFilters,
		m.cfg.FilterModLimit, m.cfg.FilterPlayersLimit, m.cfg.FilterPlayersSlots)
	m.pg = pageBrowser
	return m.buildBrowser()
}

func filterPrompt(kind filterKind, cfg *config.Config) string {
	switch kind {
	case filterByMap:
		return "ChernarusPlus"
	case filterByModName:
		return "mod name"
	case filterByModID:
		return "workshop ID"
	case filterModsLT, filterModsGT:
		return fmt.Sprintf("%d", cfg.FilterModLimit)
	case filterPlayersAbovePct, filterPlayersBelowPct:
		return fmt.Sprintf("%d", cfg.FilterPlayersLimit)
	case filterMinSlots:
		return fmt.Sprintf("%d", cfg.FilterPlayersSlots)
	}
	return ""
}

// ---- Direct Connect --------------------------------------------------

func (m Model) buildDirectConnect() (Model, tea.Cmd) {
	m.inputs[0] = textinput.New()
	m.inputs[0].Placeholder = "127.0.0.1"
	m.inputs[0].Prompt = "Server IP:   "
	m.inputs[0].CharLimit = 64
	m.inputs[0].Focus()

	m.inputs[1] = textinput.New()
	m.inputs[1].Placeholder = "2302"
	m.inputs[1].Prompt = "Game port:   "
	m.inputs[1].CharLimit = 8

	m.focusedInput = 0
	m.pg = pageDirectConnect
	return m, nil
}

func (m Model) keyDirectConnect(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "shift+tab":
		m.focusedInput = 1 - m.focusedInput
		if m.focusedInput == 0 {
			m.inputs[0].Focus()
			m.inputs[1].Blur()
		} else {
			m.inputs[1].Focus()
			m.inputs[0].Blur()
		}
		return m, nil
	case "enter":
		if m.focusedInput == 0 {
			m.focusedInput = 1
			m.inputs[1].Focus()
			m.inputs[0].Blur()
			return m, nil
		}
		ip := strings.TrimSpace(m.inputs[0].Value())
		portStr := strings.TrimSpace(m.inputs[1].Value())
		if portStr == "" {
			portStr = "2302"
		}
		var port int
		if _, err := fmt.Sscan(portStr, &port); err != nil || port == 0 {
			port = 2302
		}
		for i := range m.servers {
			s := &m.servers[i]
			if s.Endpoint.IP == ip && s.GamePort == port {
				m.sel = s
				m.pg = pageServerAction
				return m.buildServerAction(s, "", "", 0)
			}
		}
		return m.connectDirect(ip, port)
	}
	var cmd tea.Cmd
	m.inputs[m.focusedInput], cmd = m.inputs[m.focusedInput].Update(msg)
	return m, cmd
}

// connectDirect targets a server that is not in the list; since we cannot
// know whether it needs a password, ask (empty input = none).
func (m Model) connectDirect(ip string, gamePort int) (Model, tea.Cmd) {
	m.sel = nil
	m.pendingIP = ip
	m.pendingPort = gamePort
	var mm Model
	var cmd tea.Cmd
	mm, cmd = m.buildPasswordPrompt()
	mm.inputs[0].Placeholder = "leave empty if none"
	return mm, cmd
}

// ---- Server Action menu ----------------------------------------------

func (m Model) buildServerAction(srv *api.Server, offlineName, ip string, port int) (Model, tea.Cmd) {
	var labels []string
	if srv != nil {
		labels = append(labels, "Play", "Update Server Mods")
		if m.profile.IsFavorite(srv.Endpoint.IP, srv.Endpoint.Port) {
			labels = append(labels, "Remove from Favorites")
		} else {
			labels = append(labels, "Add to Favorites")
		}
		appsDir := launcher.AppsDir()
		if launcher.DesktopEntryExists(appsDir, srv.Endpoint.IP, srv.GamePort) {
			labels = append(labels, "Update Desktop Entry", "Delete Desktop Entry")
		} else {
			labels = append(labels, "Create Desktop Entry")
		}
	} else {
		labels = append(labels, "Remove from Favorites")
	}
	labels = append(labels, "Back")

	title := "Server Options"
	if srv != nil {
		title = truncate(srv.Name, 50)
	} else if offlineName != "" {
		title = offlineName + " (offline)"
	} else if ip != "" {
		title = fmt.Sprintf("%s:%d (offline)", ip, port)
	}

	m.list = newList(newMenuItems(labels...), title, m.w, m.h-4)
	return m, nil
}

func (m Model) keyServerAction(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		return m.goBackFromAction()
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i, ok := m.list.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		return m.handleServerActionSelect(i.label)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m Model) handleServerActionSelect(label string) (Model, tea.Cmd) {
	srv := m.sel
	switch label {
	case "Play":
		if srv == nil {
			return m, nil
		}
		return m.playServer(srv)
	case "Update Server Mods":
		if srv == nil {
			return m, nil
		}
		return m.updateServerMods(srv)
	case "Add to Favorites":
		if srv != nil {
			m.profile.AddFavorite(srv.Name, srv.Endpoint.IP, srv.Endpoint.Port)
			_ = m.profile.Save(m.cfg.ProfilePath)
		}
	case "Remove from Favorites":
		if srv != nil {
			m.profile.RemoveFavorite(srv.Endpoint.IP, srv.Endpoint.Port)
			_ = m.profile.Save(m.cfg.ProfilePath)
		}
	case "Create Desktop Entry", "Update Desktop Entry":
		if srv != nil {
			_ = launcher.CreateDesktopEntry(
				launcher.AppsDir(), srv.Name, srv.Endpoint.IP,
				srv.GamePort, launcher.ExecPath(),
			)
		}
	case "Delete Desktop Entry":
		if srv != nil {
			_ = launcher.DeleteDesktopEntry(launcher.AppsDir(), srv.Endpoint.IP, srv.GamePort)
		}
	case "Back":
		return m.goBackFromAction()
	}
	return m.goBackFromAction()
}

func (m Model) goBackFromAction() (Model, tea.Cmd) {
	m.pg = m.prevPage
	if m.prevPage == pageBrowser {
		return m.buildBrowser()
	}
	return m.buildMainList()
}

// playServer is the entry point of the launch flow: check mods, then
// password, then actually start the game.
func (m Model) playServer(srv *api.Server) (Model, tea.Cmd) {
	if missing := m.missingServerMods(srv); len(missing) > 0 {
		return m.buildMissingMods(missing)
	}
	return m.promptPasswordOrLaunch(srv)
}

// missingServerMods returns the server mods found in neither mod dir.
func (m Model) missingServerMods(srv *api.Server) []api.Mod {
	return missingMods(m.paths, srv)
}

// missingMods returns the mods of srv found in neither of paths' mod dirs.
func missingMods(paths *steam.Paths, srv *api.Server) []api.Mod {
	if paths == nil || srv == nil {
		return nil
	}
	var missing []api.Mod
	for _, mod := range srv.Mods {
		if _, ok := paths.FindMod(mod.SteamWorkshopID); !ok {
			missing = append(missing, mod)
		}
	}
	return missing
}

// canSteamCMDInstall reports whether the steamcmd install (and login) keys
// apply: steamcmd is the backend, or Steamworks failed and steamcmd is ready.
func (m Model) canSteamCMDInstall() bool {
	if m.backend == backendSteamCMD {
		return m.bk.steamCMDReady()
	}
	return m.swFailed && m.cfg.SteamCMDEnabled && m.bk.steamCMDReady()
}

// buildMissingMods shows the list of mods that must be installed to play.
func (m Model) buildMissingMods(missing []api.Mod) (Model, tea.Cmd) {
	m.swFailed = false
	return m.buildMissingModsNote(missing, "")
}

// buildMissingModsNote is buildMissingMods with a status line above the list.
func (m Model) buildMissingModsNote(missing []api.Mod, note string) (Model, tea.Cmd) {
	m.missingMods = missing
	m.missingNote = note
	m.pg = pageMissingMods
	m.newsView = viewport.New(m.w-4, m.h-6)
	cmd := m.refreshBackend()

	var sb strings.Builder
	if note != "" {
		sb.WriteString(note + "\n\n")
	}
	sb.WriteString(dimStyle.Render("The server runs mods you don't have installed:") + "\n\n")
	for _, mod := range missing {
		fmt.Fprintf(&sb, "  %s %s\n", redStyle.Render("✗"), mod.Name)
		if !steam.ValidModID(mod.SteamWorkshopID) {
			sb.WriteString(redStyle.Render(fmt.Sprintf("    invalid workshop ID %q from the server; cannot be installed", mod.SteamWorkshopID)) + "\n")
			continue
		}
		sb.WriteString(dimStyle.Render("    "+mod.WorkshopURL()) + "\n")
	}
	sb.WriteString("\n" + dimStyle.Render("Mod backend: "+m.backendLabel()) + "\n")
	if m.backendWhy != "" {
		sb.WriteString(dimStyle.Render(m.backendWhy) + "\n")
	}
	if m.backend == backendSteamworks {
		// Shown so a stand-in copy (e.g. an emulator's) is easy to spot.
		sb.WriteString(dimStyle.Render("libsteam_api.so: "+m.steamAPIStatus()) + "\n")
	}
	sb.WriteString("\n")
	switch {
	case m.backend == backendPending:
		sb.WriteString(dimStyle.Render("Looking for the Steam API library (libsteam_api.so); "+
			"installing is possible once that is done.") + "\n")
	case m.backend == backendSteamworks && !m.swFailed:
		sb.WriteString(dimStyle.Render("Press i to subscribe and download them with your running Steam client.\n"+
			"Steam shows DayZ as running while it downloads.") + "\n")
	case m.backend == backendSteamCMD:
		if p := m.bk.steamCMDProblem(); p != "" {
			sb.WriteString(redStyle.Render("SteamCMD: "+p) + "\n")
		} else {
			sb.WriteString(dimStyle.Render("Press i to install them with SteamCMD (into dzman's own folder).\n"+
				"Press l first to log in to SteamCMD if it has never logged in.") + "\n")
		}
	}
	if m.backend == backendSteamworks && m.swFailed {
		sb.WriteString(dimStyle.Render("Press i to try Steam again.") + "\n")
		if m.canSteamCMDInstall() {
			sb.WriteString(dimStyle.Render("Press c to install them with SteamCMD instead.") + "\n")
		}
	}
	sb.WriteString(dimStyle.Render("Press o to open the Workshop pages and subscribe in Steam yourself, then try again.") + "\n")
	m.newsView.SetContent(sb.String())
	return m, cmd
}

func (m Model) keyMissingMods(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "i":
		switch m.backend {
		case backendSteamworks:
			ids, names := modNames(m.missingMods)
			return m.startSteamworks(dlInstall, ids, names)
		case backendSteamCMD:
			return m.installWithSteamCMD()
		}
		return m, nil
	case "c":
		if time.Now().Before(m.swallowC) {
			return m, nil
		}
		if m.backend == backendSteamworks && m.canSteamCMDInstall() {
			return m.installWithSteamCMD()
		}
		return m, nil
	case "l":
		if !m.canSteamCMDInstall() {
			return m, nil
		}
		return m, m.steamLoginCmd()
	case "o":
		return m, openWorkshopCmd(m.missingMods)
	case "esc", "q":
		m.pg = pageServerAction
		return m.buildServerAction(m.sel, "", "", 0)
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.newsView, cmd = m.newsView.Update(msg)
	return m, cmd
}

// installWithSteamCMD runs the steamcmd install for the missing mods.
func (m Model) installWithSteamCMD() (Model, tea.Cmd) {
	if !m.bk.steamCMDReady() || m.paths == nil {
		return m, nil
	}
	m.loading = true
	m.loadingMsg = fmt.Sprintf("Installing %d mods with SteamCMD…", len(m.missingMods))
	m.pg = pageLoading
	return m, tea.Batch(m.spin.Tick, installMissingModsCmd(
		m.bk.cmdPath, m.profile.SteamLogin, m.paths, m.missingMods,
	))
}

// promptPasswordOrLaunch asks for the server password when needed.
func (m Model) promptPasswordOrLaunch(srv *api.Server) (Model, tea.Cmd) {
	if !srv.Password {
		return m.doLaunch(srv, "")
	}
	return m.buildPasswordPrompt()
}

func (m Model) buildPasswordPrompt() (Model, tea.Cmd) {
	m.pg = pagePassword
	m.inputs[0] = textinput.New()
	m.inputs[0].Prompt = "Password: "
	m.inputs[0].EchoMode = textinput.EchoPassword
	m.inputs[0].CharLimit = 128
	m.inputs[0].Focus()
	return m, nil
}

func (m Model) keyPassword(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		password := m.inputs[0].Value()
		if m.sel != nil {
			return m.doLaunch(m.sel, password)
		}
		return m.doLaunchDirect(m.pendingIP, m.pendingPort, password)
	case "esc":
		if m.sel != nil {
			m.pg = pageServerAction
			return m.buildServerAction(m.sel, "", "", 0)
		}
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.inputs[0], cmd = m.inputs[0].Update(msg)
	return m, cmd
}

// doLaunch links the mods, records history, and starts the game.
func (m Model) doLaunch(srv *api.Server, password string) (Model, tea.Cmd) {
	args, err := prepareLaunch(m.cfg, m.profile, m.paths, srv, password)
	if err != nil {
		m.err = err
		m.pg = pageError
		return m, nil
	}
	m.loading = true
	m.loadingMsg = "Launching DayZ…"
	m.pg = pageLoading
	return m, tea.Batch(m.spin.Tick, launchServerCmd(srv, m.profile.Player, args))
}

// prepareLaunch links srv's mods into the DayZ folder and returns the
// arguments to start DayZ with. It is the part of a launch the TUI and the
// terminal join share. The join is recorded by recordJoin once DayZ started.
func prepareLaunch(cfg *config.Config, profile *config.Profile, paths *steam.Paths,
	srv *api.Server, password string) ([]string, error) {
	modIDs := make([]string, len(srv.Mods))
	for i, mod := range srv.Mods {
		modIDs[i] = mod.SteamWorkshopID
	}
	if bad := steam.InvalidModIDs(modIDs); len(bad) > 0 {
		return nil, fmt.Errorf("invalid workshop IDs from the server: %q", bad)
	}
	if paths != nil {
		if err := steam.EnsureSymlinks(paths.DayZ, paths.ModDirs(), modIDs); err != nil {
			return nil, err
		}
	}

	args := launcher.ConnectArgs{
		ServerIP:   srv.Endpoint.IP,
		GamePort:   srv.GamePort,
		Password:   password,
		ModIDs:     modIDs,
		PlayerName: profile.Player,
		ExtraArgs:  profile.LaunchArgs(),
	}
	return args.ToSteamArgs(), nil
}

// recordJoin adds srv to the history and saves the profile. It runs after
// DayZ was started, so a launch that fails leaves the history alone.
func recordJoin(cfg *config.Config, profile *config.Profile, srv *api.Server) {
	profile.AddToHistory(srv.Name, srv.Endpoint.IP, srv.Endpoint.Port, cfg.HistorySize)
	_ = profile.Save(cfg.ProfilePath)
}

// doLaunchDirect launches against an address that is not in the server list.
func (m Model) doLaunchDirect(ip string, gamePort int, password string) (Model, tea.Cmd) {
	args := launcher.ConnectArgs{
		ServerIP:   ip,
		GamePort:   gamePort,
		Password:   password,
		PlayerName: m.profile.Player,
		ExtraArgs:  m.profile.LaunchArgs(),
	}
	m.loading = true
	m.loadingMsg = "Launching DayZ…"
	m.pg = pageLoading
	return m, tea.Batch(m.spin.Tick, launchDayZCmd(m.profile.Player, args.ToSteamArgs()))
}

func (m Model) launchGame() (Model, tea.Cmd) {
	m.loading = true
	m.loadingMsg = "Launching DayZ…"
	m.pg = pageLoading
	return m, tea.Batch(m.spin.Tick, launchDayZCmd(m.profile.Player, m.profile.LaunchArgs()))
}

// ---- Offline mission -------------------------------------------------

func (m Model) buildOfflineMission() (Model, tea.Cmd) {
	m.pg = pageOfflineMission
	if m.paths == nil {
		m.err = fmt.Errorf("DayZ path not found")
		m.pg = pageError
		return m, nil
	}
	missions, err := launcher.AvailableMissions(m.paths.DayZ)
	if err != nil || len(missions) == 0 {
		m.err = fmt.Errorf("no offline missions found — install DayZCommunityOfflineMode first")
		m.pg = pageError
		return m, nil
	}
	labels := make([]string, len(missions))
	for i, mis := range missions {
		labels[i] = mis.Name
	}
	m.list = newList(newMenuItems(labels...), "Offline Mission", m.w, m.h-4)
	return m, nil
}

func (m Model) keyOfflineMission(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i, ok := m.list.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		return m.launchOffline(i.label)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m Model) launchOffline(missionName string) (Model, tea.Cmd) {
	missionPath := filepath.Join(m.paths.DayZ, "Missions", missionName)
	_ = launcher.SetHiveEnabled(missionPath, true)

	extraArgs := append(m.profile.LaunchArgs(),
		"-filePatching",
		"-mission=./Missions/"+missionName,
		"-doLogs",
		"-scriptDebug=true",
	)
	if err := launcher.LaunchDayZ(m.profile.Player, extraArgs); err != nil {
		m.err = err
		m.pg = pageError
		return m, nil
	}
	return m, tea.Quit
}

// ---- News ------------------------------------------------------------

func (m Model) buildNews() (Model, tea.Cmd) {
	m.pg = pageNews
	m.newsView = viewport.New(m.w-4, m.h-6)

	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Latest DayZ News") + "\n\n")
	for i, a := range m.news {
		if i >= 20 {
			break
		}
		sb.WriteString(subtitleStyle.Render(fmt.Sprintf("%d. %s", i+1, a.Title)) + "\n")
		url := fmt.Sprintf("https://dayz.com/article/%s/%s", a.ArticleCategory.Slug, a.Slug)
		sb.WriteString(dimStyle.Render("   "+url) + "\n\n")
	}
	m.newsView.SetContent(sb.String())
	return m, nil
}

func (m Model) keyNews(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		if m.resultToServer {
			m.resultToServer = false
			m.pg = pageServerAction
			m, cmd := m.buildServerAction(m.sel, "", "", 0)
			m.list.Select(1) // keep the cursor on Update Server Mods
			return m, cmd
		}
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.newsView, cmd = m.newsView.Update(msg)
	return m, cmd
}

// ---- Installed mods --------------------------------------------------

func (m Model) buildModInfo() (Model, tea.Cmd) {
	m.pg = pageModInfo
	if len(m.mods) == 0 {
		m.list = newList(newMenuItems("No mods installed"), "Installed Mods", m.w, m.h-4)
		return m, nil
	}
	items := make([]list.Item, len(m.mods))
	for i, mod := range m.mods {
		items[i] = modItem{mod: mod}
	}
	m.list = newList(items, fmt.Sprintf("Installed Mods (%d)", len(m.mods)), m.w, m.h-4)
	return m, nil
}

func (m Model) keyModInfo(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// ---- Config menu -----------------------------------------------------

func (m Model) buildConfigMenu() (Model, tea.Cmd) {
	m.pg = pageConfig
	cmd := m.refreshBackend()
	labels := []string{"Launch Options", "Change Player Name", "Change Steam Root",
		"Mod Backend", "Change Steam API Path"}
	if m.cfg.SteamCMDEnabled {
		labels = append(labels, "Change Steam Login", "Change SteamCMD Path", "Disable SteamCMD")
	} else {
		labels = append(labels, "Enable SteamCMD")
	}
	clientIDs, _ := m.clientMods()
	managed := m.managedMods()
	if len(managed) > 0 {
		labels = append(labels, "Remove Managed Mods")
	}
	if len(m.mods) > 0 {
		labels = append(labels, "Remove All Mod Links")
	}
	updateDesc := m.updateBlockedWhy()
	if updateDesc == "" {
		switch m.backend {
		case backendSteamworks:
			updateDesc = fmt.Sprintf("ask the Steam client to update %d workshop mods (unsubscribed ones are skipped)", len(clientIDs))
		case backendSteamCMD:
			updateDesc = fmt.Sprintf("re-download %d dzman SteamCMD mods", len(managed))
		}
	}
	labels = append(labels, "Update Mods")
	labels = append(labels, "Refresh Server List", "Check for Updates", "About", "Back")
	items := newMenuItems(labels...)
	for i, it := range items {
		mi := it.(menuItem)
		switch mi.label {
		case "Change SteamCMD Path":
			mi.desc = "steamcmd: " + m.steamCMDStatus()
		case "Mod Backend":
			mi.desc = m.backendLabel() + "  (enter to change)"
			if !m.cfg.SteamCMDEnabled {
				mi.desc += "; enable SteamCMD to offer steamcmd"
			}
		case "Change Steam API Path":
			mi.desc = "libsteam_api.so: " + m.steamAPIStatus()
		case "Remove Managed Mods":
			mi.desc = fmt.Sprintf("delete %d mods dzman downloaded with SteamCMD; Steam's own mods are kept", len(managed))
		case "Update Mods":
			mi.desc = updateDesc
		}
		items[i] = mi
	}
	m.list = newList(items, "Config", m.w, m.h-4)
	return m, cmd
}

// rebuildConfigMenu rebuilds the config menu keeping the cursor on the same
// entry, or at the same position if that entry is gone.
func (m Model) rebuildConfigMenu() (Model, tea.Cmd) {
	idx := m.list.Index()
	label := ""
	if it, ok := m.list.SelectedItem().(menuItem); ok {
		label = it.label
	}
	m, cmd := m.buildConfigMenu()
	items := m.list.Items()
	for i, it := range items {
		if mi, ok := it.(menuItem); ok && mi.label == label {
			idx = i
			break
		}
	}
	if idx >= len(items) {
		idx = len(items) - 1
	}
	m.list.Select(max(idx, 0))
	return m, cmd
}

// steamCMDStatus describes the configured steamcmd path and what it resolves
// to, e.g. "steamcmd → /home/x/steamcmd/steamcmd.sh" or "steamcmd (not found)".
func (m Model) steamCMDStatus() string {
	shown := m.cfg.SteamCMDPath
	if shown == "" {
		shown = steam.DefaultSteamCMD
	}
	path, err := m.resolveSteamCMD()
	if err != nil {
		return shown + " (not found)"
	}
	return shown + " → " + path
}

func (m Model) keyConfig(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageMain
		return m.buildMainList()
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		i, ok := m.list.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		return m.handleConfigSelect(i.label)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m Model) handleConfigSelect(label string) (Model, tea.Cmd) {
	switch label {
	case "Launch Options":
		return m.buildLaunchOpts()
	case "Change Player Name":
		m.inputs[0] = textinput.New()
		m.inputs[0].Placeholder = m.profile.Player
		m.inputs[0].Prompt = "Player name: "
		m.inputs[0].SetValue(m.profile.Player)
		m.inputs[0].Focus()
		m.pg = pageSetup
		m.step = setupPlayer
		return m, nil
	case "Change Steam Root":
		m.inputs[0] = textinput.New()
		m.inputs[0].Placeholder = "~/.steam/steam"
		m.inputs[0].Prompt = "Steam root: "
		m.inputs[0].Focus()
		m.pg = pageSetup
		m.step = setupSteamDir
		return m, nil
	case "Change Steam Login":
		m.inputs[0] = textinput.New()
		m.inputs[0].Placeholder = m.profile.SteamLogin
		m.inputs[0].Prompt = "Steam login: "
		m.inputs[0].SetValue(m.profile.SteamLogin)
		m.inputs[0].Focus()
		m.pg = pageSetup
		m.step = setupLogin
		return m, nil
	case "Change SteamCMD Path":
		m.inputs[0] = textinput.New()
		m.inputs[0].Placeholder = "steamcmd"
		m.inputs[0].Prompt = "SteamCMD path: "
		m.inputs[0].SetValue(m.cfg.SteamCMDPath)
		m.inputs[0].Focus()
		m.pg = pageSetup
		m.step = setupSteamCMDPath
		return m, nil
	case "Mod Backend":
		m.cfg.ModBackend = nextBackend(m.cfg.Backend(), m.cfg.SteamCMDEnabled)
		_ = m.cfg.Save()
		sel := m.list.Index()
		m, cmd := m.buildConfigMenu()
		m.list.Select(sel)
		return m, cmd
	case "Change Steam API Path":
		m.inputs[0] = textinput.New()
		m.inputs[0].Placeholder = "empty = auto-detect"
		m.inputs[0].Prompt = "libsteam_api.so path: "
		m.inputs[0].SetValue(m.cfg.SteamAPIPath)
		m.inputs[0].Focus()
		m.pg = pageSetup
		m.step = setupSteamAPIPath
		return m, nil
	case "Disable SteamCMD":
		m.cfg.SteamCMDEnabled = false
		_ = m.cfg.Save()
		return m.buildConfigMenu()
	case "Enable SteamCMD":
		m.cfg.SteamCMDEnabled = true
		_ = m.cfg.Save()
		return m.buildConfigMenu()
	case "Remove Managed Mods":
		if m.paths != nil {
			_, _ = steam.RemoveManagedMods(m.paths)
			return m, updateModsCmd(m.paths)
		}
	case "Remove All Mod Links":
		if m.paths != nil {
			_, _ = steam.RemoveSymlinks(m.paths.DayZ)
		}
		return m.buildConfigMenu()
	case "Update Mods":
		if why := m.updateBlockedWhy(); why != "" {
			return m.showUpdateError(errors.New(why))
		}
		switch m.backend {
		case backendSteamworks:
			ids, names := m.clientMods()
			return m.startSteamworks(dlUpdate, ids, names)
		case backendSteamCMD:
			if !m.bk.steamCMDReady() {
				return m.showUpdateError(errors.New("SteamCMD: " + m.bk.steamCMDProblem()))
			}
			m.loading = true
			m.loadingMsg = "Updating SteamCMD mods…"
			m.pg = pageLoading
			return m, tea.Batch(m.spin.Tick, steamCMDUpdateCmd(m.bk.cmdPath, m.profile.SteamLogin,
				m.paths.CmdContent, m.managedMods()))
		}
	case "Refresh Server List":
		m.loading = true
		m.loadingMsg = "Refreshing server list…"
		m.pg = pageLoading
		m.loadedSrv, m.loadedNews, m.loadedMods = false, false, false
		return m, tea.Batch(m.spin.Tick,
			m.fetchServersCmd(true), m.fetchNewsCmd(true), m.scanModsCmd())
	case "Check for Updates":
		m.loading = true
		m.loadingMsg = "Checking for updates…"
		m.pg = pageLoading
		return m, tea.Batch(m.spin.Tick, checkUpdateCmd(m.cfg.UserAgent))
	case "About":
		return m.buildAbout()
	case "Back":
		m.pg = pageMain
		return m.buildMainList()
	}
	return m, nil
}

// showUpdateResult renders the GitHub release check on the viewport page.
func (m Model) showUpdateResult(msg updateCheckMsg) (Model, tea.Cmd) {
	m.loading = false
	m.pg = pageNews // reuse the scrollable viewport page
	m.newsView = viewport.New(m.w-4, m.h-6)

	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Update Check") + "\n\n")
	switch {
	case msg.err != nil:
		sb.WriteString(redStyle.Render("  Check failed: "+msg.err.Error()) + "\n")
	case msg.latest == "":
		sb.WriteString(dimStyle.Render("  No releases published yet.") + "\n")
	case strings.TrimPrefix(msg.latest, "v") == strings.TrimPrefix(config.AppVersion, "v"):
		sb.WriteString(greenStyle.Render(
			fmt.Sprintf("  You are up to date (%s).", config.AppVersion)) + "\n")
	default:
		fmt.Fprintf(&sb, "  Installed: %s\n  Latest:    %s\n\n",
			config.AppVersion, msg.latest)
		sb.WriteString(dimStyle.Render("  https://github.com/"+api.Repo+"/releases") + "\n")
	}
	m.newsView.SetContent(sb.String())
	return m, nil
}

func (m Model) buildAbout() (Model, tea.Cmd) {
	m.pg = pageNews // reuse news viewport for about text
	m.newsView = viewport.New(m.w-4, m.h-6)
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("dzman "+config.AppVersion) + "\n\n")
	sb.WriteString(subtitleStyle.Render("Paths") + "\n")
	sb.WriteString(dimStyle.Render("  Config:   "+m.cfg.ConfPath) + "\n")
	sb.WriteString(dimStyle.Render("  Profile:  "+m.cfg.ProfilePath) + "\n")
	if m.paths != nil {
		sb.WriteString(dimStyle.Render("  DayZ:     "+m.paths.DayZ) + "\n")
		sb.WriteString(dimStyle.Render("  Workshop: "+m.paths.Workshop) + "\n")
		if m.paths.CmdWorkshop != "" {
			sb.WriteString(dimStyle.Render("  SteamCMD: "+m.paths.CmdWorkshop) + "\n")
		}
	}
	sb.WriteString("\n" + subtitleStyle.Render("Settings") + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  Player:     %s", m.profile.Player)) + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  Steam login: %s", m.profile.SteamLogin)) + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  SteamCMD:   %v", m.cfg.SteamCMDEnabled)) + "\n")
	cmd := m.refreshBackend()
	sb.WriteString(dimStyle.Render("  Mod backend: "+m.backendLabel()) + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  Server TTL: %s", m.cfg.ServerDBTTL)) + "\n")
	sb.WriteString(dimStyle.Render(fmt.Sprintf("  API:        %s", m.cfg.APIBaseURL)) + "\n")
	sb.WriteString("\n" + dimStyle.Render("Thanks to dayzsalauncher.com for the API") + "\n")
	m.newsView.SetContent(sb.String())
	return m, cmd
}

// ---- Launch options --------------------------------------------------

func (m Model) buildLaunchOpts() (Model, tea.Cmd) {
	m.pg = pageLaunchOpts
	order := []string{
		"nosplash", "skipintro", "window", "noborder", "high",
		"USEALLAVAILABLECORES", "noBenchmark", "filePatching",
		"doLogs", "buldozer", "world", "noPause",
		"maxMem", "maxVRAM", "cpuCount", "exThreads", "scriptDebug",
	}
	items := make([]list.Item, 0, len(order))
	m.multiSel = make([]bool, 0, len(order))
	for _, k := range order {
		opt, ok := m.profile.Options[k]
		if !ok {
			continue
		}
		val := ""
		hasVal := opt.Value != nil
		if hasVal {
			val = *opt.Value
		}
		items = append(items, launchOptItem{
			key:     k,
			enabled: opt.Enabled,
			desc:    opt.Description,
			hasVal:  hasVal,
			val:     val,
		})
		m.multiSel = append(m.multiSel, opt.Enabled)
	}
	m.list = newList(items, "Launch Options  [space] toggle", m.w, m.h-4)
	m.list.SetFilteringEnabled(false)
	return m, nil
}

func (m Model) keyLaunchOpts(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.pg = pageConfig
		return m.buildConfigMenu()
	case "ctrl+c":
		return m, tea.Quit
	case " ":
		idx := m.list.Index()
		if idx < 0 || idx >= len(m.multiSel) {
			return m, nil
		}
		m.multiSel[idx] = !m.multiSel[idx]
		item, ok := m.list.Items()[idx].(launchOptItem)
		if !ok {
			return m, nil
		}
		item.enabled = m.multiSel[idx]
		opt := m.profile.Options[item.key]
		opt.Enabled = item.enabled
		m.profile.Options[item.key] = opt
		_ = m.profile.Save(m.cfg.ProfilePath)
		return m, m.list.SetItem(idx, item)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// ---- Detail panel ----------------------------------------------------

func (m *Model) refreshDetail() {
	if m.sel == nil {
		return
	}
	s := m.sel
	var sb strings.Builder

	sb.WriteString(titleStyle.Render(s.Name) + "\n\n")

	row := func(k, v string) {
		fmt.Fprintf(&sb, "  %-18s %s\n", dimStyle.Render(k+":"), v)
	}

	sb.WriteString(subtitleStyle.Render("Status") + "\n")
	row("Players", fmt.Sprintf("%d / %d", s.Players, s.MaxPlayers))
	row("Map", s.Map)
	row("Time", s.Time+" "+timeEmoji(s.Time))
	row("Time Accel", fmt.Sprintf("x%.1f", s.TimeAcceleration))
	row("Password", boolIcon(s.Password))
	row("BattlEye", boolIcon(s.BattlEye))
	row("VAC", boolIcon(s.VAC))
	row("First Person", boolIcon(s.FirstPersonOnly))
	row("Official", boolIcon(s.Shard == "public"))
	row("Platform", envIcon(s.Environment))
	row("Version", s.Version)

	sb.WriteString("\n" + subtitleStyle.Render("Network") + "\n")
	row("IP", s.Endpoint.IP)
	row("Game Port", fmt.Sprintf("%d", s.GamePort))
	row("Query Port", fmt.Sprintf("%d", s.Endpoint.Port))
	row("Country", m.selCountry)
	row("Ping", m.selPing)
	if m.selLive != nil {
		// Queried directly from the server just now, unlike the API snapshot.
		row("Live Players", greenStyle.Render(
			fmt.Sprintf("%d / %d", m.selLive.Players, m.selLive.MaxPlayers)))
	}
	if m.selBMURL != "" {
		row("Battlemetrics", dimStyle.Render(m.selBMURL))
	}

	if len(s.Mods) > 0 {
		fmt.Fprintf(&sb, "\n%s\n", subtitleStyle.Render(fmt.Sprintf("Mods (%d)", len(s.Mods))))
		for _, mod := range s.Mods {
			icon := redStyle.Render("✗")
			if m.paths != nil {
				if _, ok := m.paths.FindMod(mod.SteamWorkshopID); ok {
					icon = greenStyle.Render("✓")
				}
			}
			fmt.Fprintf(&sb, "  %s %s\n", icon, truncate(mod.Name, 40))
		}
	}

	m.detail.SetContent(sb.String())
}

// ---- View ------------------------------------------------------------

// View renders the current page.
func (m Model) View() string {
	switch m.pg {
	case pageLoading:
		return m.viewLoading()
	case pageSetup:
		return m.viewSetup()
	case pageMain:
		return m.viewMain()
	case pageBrowser:
		return m.viewBrowser()
	case pageServerAction, pageFilterMenu, pageConfig,
		pageLaunchOpts, pageModInfo, pageOfflineMission:
		return m.viewList()
	case pageFilterInput, pageDirectConnect:
		return m.viewInputForm()
	case pageNews:
		return m.viewNews()
	case pageMissingMods:
		return m.viewMissingMods()
	case pageModDownload:
		return m.viewModDownload()
	case pagePassword:
		return m.viewPassword()
	case pageError:
		return m.viewError()
	}
	return ""
}

func (m Model) viewMissingMods() string {
	help := "o open Workshop pages  esc back"
	switch {
	case m.backend == backendPending:
		help = pendingLabel + "  " + help
	case m.backend == backendSteamworks && m.canSteamCMDInstall():
		help = "i retry with Steam  c install via SteamCMD  l log in to SteamCMD  " + help
	case m.backend == backendSteamworks && m.swFailed:
		help = "i retry with Steam  " + help
	case m.backend == backendSteamworks:
		help = "i download with Steam  " + help
	case m.canSteamCMDInstall():
		help = "i install via SteamCMD  l log in to SteamCMD  " + help
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		headerStyle.Render(fmt.Sprintf("Missing Mods (%d)", len(m.missingMods))),
		m.newsView.View(),
		helpStyle.Render("  "+help),
	)
}

func (m Model) viewPassword() string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Server Password") + "\n\n")
	if m.sel != nil {
		sb.WriteString(dimStyle.Render("  "+truncate(m.sel.Name, m.w-4)) + "\n\n")
	}
	sb.WriteString(m.inputs[0].View() + "\n\n")
	sb.WriteString(helpStyle.Render("enter connect  esc back"))
	return sb.String()
}

func (m Model) viewLoading() string {
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center,
		fmt.Sprintf("%s  %s", m.spin.View(), dimStyle.Render(m.loadingMsg)),
	)
}

func (m Model) viewSetup() string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("dzman — First Run Setup") + "\n\n")
	switch m.step {
	case setupWelcome:
		sb.WriteString(m.list.View())
	case setupLoginNow:
		sb.WriteString(m.list.View())
	case setupLogin:
		sb.WriteString(subtitleStyle.Render("Steam Login") + "\n\n")
		if m.setupNote != "" {
			sb.WriteString(redStyle.Render(m.setupNote) + "\n\n")
		}
		sb.WriteString(m.inputs[0].View() + "\n\n")
		sb.WriteString(helpStyle.Render("enter to confirm"))
	case setupPlayer:
		sb.WriteString(subtitleStyle.Render("Player Name") + "\n\n")
		if m.setupNote != "" {
			sb.WriteString(dimStyle.Render(m.setupNote) + "\n\n")
		}
		sb.WriteString(m.inputs[0].View() + "\n\n")
		sb.WriteString(helpStyle.Render("enter to confirm"))
	case setupSteamDir:
		sb.WriteString(subtitleStyle.Render("Steam Root Directory") + "\n\n")
		sb.WriteString(dimStyle.Render("e.g. ~/.steam/steam or ~/.local/share/Steam") + "\n\n")
		sb.WriteString(m.inputs[0].View() + "\n\n")
		sb.WriteString(helpStyle.Render("enter to confirm"))
	case setupSteamCMDPath:
		sb.WriteString(subtitleStyle.Render("SteamCMD Path") + "\n\n")
		sb.WriteString(dimStyle.Render("e.g. /usr/bin/steamcmd or ~/steamcmd/steamcmd.sh") + "\n")
		sb.WriteString(dimStyle.Render("Currently: "+m.steamCMDStatus()) + "\n\n")
		sb.WriteString(m.inputs[0].View() + "\n\n")
		sb.WriteString(helpStyle.Render("enter to confirm, leave blank to auto-detect (steamcmd / steamcmd.sh), esc to cancel"))
	case setupSteamAPIPath:
		sb.WriteString(subtitleStyle.Render("Steamworks Library") + "\n\n")
		sb.WriteString(dimStyle.Render("A 64-bit Linux libsteam_api.so, e.g. from the Steamworks SDK's") + "\n")
		sb.WriteString(dimStyle.Render("redistributable_bin/linux64/. Or copy it to "+m.cfg.Dir+"/.") + "\n")
		sb.WriteString(dimStyle.Render("Currently: "+m.steamAPIStatus()) + "\n\n")
		sb.WriteString(m.inputs[0].View() + "\n\n")
		sb.WriteString(helpStyle.Render("enter to confirm, leave blank to auto-detect, esc to cancel"))
	}
	return sb.String()
}

func (m Model) viewMain() string {
	var sb strings.Builder
	sb.WriteString(headerStyle.Render("dzman "+config.AppVersion) + "\n")
	if len(m.servers) > 0 {
		sb.WriteString(dimStyle.Render(fmt.Sprintf(
			"  Servers: %s   Players: %s",
			greenStyle.Render(fmt.Sprintf("%d", len(m.servers))),
			greenStyle.Render(fmt.Sprintf("%d", m.totalPlayers)),
		)) + "\n")
	}
	sb.WriteString(dimStyle.Render(
		"  Player: "+subtitleStyle.Render(m.profile.Player),
	) + "\n\n")
	for i, a := range m.news {
		if i >= 3 {
			break
		}
		sb.WriteString(dimStyle.Render("  » "+truncate(a.Title, m.w-6)) + "\n")
	}
	if len(m.news) > 0 {
		sb.WriteString("\n")
	}
	if m.mainNote != "" {
		sb.WriteString(redStyle.Render("  "+m.mainNote) + "\n")
	}
	sb.WriteString(m.list.View() + "\n")
	sb.WriteString(helpStyle.Render("  ↑↓ navigate  enter select  q quit"))
	return sb.String()
}

func (m Model) viewBrowser() string {
	help := helpStyle.Render("  ↑↓/jk navigate  enter select  f favorite  / search  esc back")
	if m.w >= 120 {
		listW := m.w / 2
		detailW := m.w - listW - 4
		listPane := panelStyle.Width(listW - 2).Render(m.list.View())
		detailPane := panelStyle.Width(detailW).Render(m.detail.View())
		top := lipgloss.JoinHorizontal(lipgloss.Top, listPane, "  ", detailPane)
		return lipgloss.JoinVertical(lipgloss.Left, top, help)
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.list.View(), help)
}

func (m Model) viewList() string {
	help := helpStyle.Render("  ↑↓ navigate  enter select  esc back  q quit")
	if m.pg == pageFilterMenu || m.pg == pageLaunchOpts {
		help = helpStyle.Render("  ↑↓ navigate  space toggle  enter confirm  esc back")
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.list.View(), help)
}

func (m Model) viewInputForm() string {
	title := "Direct Connect"
	if m.pg == pageFilterInput {
		title = "Filter Value"
	}
	var sb strings.Builder
	sb.WriteString(titleStyle.Render(title) + "\n\n")
	sb.WriteString(m.inputs[0].View() + "\n")
	if m.pg == pageDirectConnect {
		sb.WriteString(m.inputs[1].View() + "\n")
		sb.WriteString("\n" + helpStyle.Render("tab switch field  enter connect  esc back"))
	} else {
		sb.WriteString("\n" + helpStyle.Render("enter confirm  esc cancel"))
	}
	return sb.String()
}

func (m Model) viewNews() string {
	help := helpStyle.Render("  ↑↓/pgup/pgdn scroll  esc back")
	return lipgloss.JoinVertical(lipgloss.Left,
		headerStyle.Render("DayZ News"),
		m.newsView.View(),
		help,
	)
}

func (m Model) viewError() string {
	msg := "unknown error"
	if m.err != nil {
		msg = m.err.Error()
	}
	box := errorBoxStyle.Render("Error: " + msg)
	help := helpStyle.Render("  enter/q to quit")
	return lipgloss.JoinVertical(lipgloss.Left,
		"\n",
		lipgloss.PlaceHorizontal(m.w, lipgloss.Center, box),
		"\n",
		help,
	)
}

// ---- Helpers ---------------------------------------------------------

func (m *Model) resizeComponents() {
	m.list.SetSize(m.w, m.h-6)
	m.newsView = viewport.New(m.w-4, m.h-6)
	if m.w >= 120 {
		m.detail = viewport.New(m.w/2-4, m.h-4)
	}
}

// newList creates a styled bubbles list with the given items and dimensions.
func newList(items []list.Item, title string, w, h int) list.Model {
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(colAccent)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(lipgloss.Color("#DDDDDD")).
		Background(colAccent)

	l := list.New(items, delegate, w, h)
	l.Title = title
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(true)
	return l
}

// humanDuration formats a duration as "Xd ago", "Xh ago", etc.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
