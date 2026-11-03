package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braydencw1/dzman/internal/api"
	"github.com/braydencw1/dzman/internal/config"
	"github.com/braydencw1/dzman/internal/launcher"
	"github.com/braydencw1/dzman/internal/procs"
	"github.com/braydencw1/dzman/internal/steam"
	"github.com/braydencw1/dzman/internal/steamworks"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- Backend selection -----------------------------------------------

// backendKind is the mod backend actually in use.
type backendKind int

const (
	backendManual backendKind = iota
	backendSteamworks
	backendSteamCMD
	// backendPending: the choice depends on the libsteam_api.so lookup,
	// which is still running. Nothing can be installed until it is known.
	backendPending
)

// pendingLabel is what backendLabel shows for backendPending.
const pendingLabel = "looking for Steam API library…"

func (b backendKind) String() string {
	switch b {
	case backendSteamworks:
		return config.BackendSteamworks
	case backendSteamCMD:
		return config.BackendSteamCMD
	case backendPending:
		return pendingLabel
	}
	return config.BackendManual
}

// backendInputs is everything resolveBackend looks at.
type backendInputs struct {
	mode         string // cfg.Backend()
	lib          string // libsteam_api.so, when found
	libErr       error
	steamRunning bool
	flatpak      bool // the Steam client is Flatpak Steam
	cmdEnabled   bool
	cmdPath      string // isolated steamcmd, when found
	cmdErr       error
	loginOK      bool // a non-anonymous steamcmd login is set
}

// steamCMDReady reports whether steamcmd can be used at all, ignoring the
// enabled setting.
func (in backendInputs) steamCMDReady() bool { return in.cmdErr == nil && in.loginOK }

// steamCMDProblem says why steamcmd cannot be used, "" if it can.
func (in backendInputs) steamCMDProblem() string {
	switch {
	case in.cmdErr != nil:
		return in.cmdErr.Error()
	case !in.loginOK:
		return "steamcmd needs a Steam login (Config > Change Steam Login); anonymous cannot download DayZ mods"
	}
	return ""
}

// resolveBackend picks the backend for in.mode and, for auto, says why the
// preferred one was skipped ("" when nothing was skipped). While the library
// lookup is pending it returns backendPending wherever the answer depends
// on it.
func resolveBackend(in backendInputs) (backendKind, string) {
	libPending := errors.Is(in.libErr, errLibPending)
	switch in.mode {
	case config.BackendSteamworks:
		if libPending {
			return backendPending, ""
		}
		return backendSteamworks, ""
	case config.BackendSteamCMD:
		if !in.cmdEnabled {
			return backendManual, "SteamCMD is disabled (Config > Enable SteamCMD)"
		}
		return backendSteamCMD, ""
	case config.BackendManual:
		return backendManual, ""
	}
	var why string
	switch {
	case in.flatpak:
		// The helper runs on the host and cannot reach a sandboxed client.
		why = "Steamworks skipped: Flatpak Steam's client can't be reached from outside its sandbox " +
			"(set Mod Backend to steamworks to try anyway)"
	case in.libErr != nil && !libPending:
		why = "Steamworks unavailable: " + in.libErr.Error()
	case !in.steamRunning:
		why = "Steamworks unavailable: the Steam client is not running"
	case libPending:
		return backendPending, ""
	default:
		return backendSteamworks, ""
	}
	if in.cmdEnabled && in.steamCMDReady() {
		return backendSteamCMD, why
	}
	return backendManual, why
}

// libLookup is a cached FindLibrary result. FindLibrary parses the ELF of
// every game's libsteam_api.so, so it runs in the background (findLibraryCmd)
// at startup and again only when its inputs change or the user sets the
// path, never on the UI goroutine.
type libLookup struct {
	key     string // libKey of the inputs; "" means not looked up
	path    string
	err     error
	pending bool // a lookup is running; nothing else is known
}

// errLibPending is the lookup error while a lookup is running.
var errLibPending = errors.New("looking for libsteam_api.so…")

// libFindOptions are FindLibrary's inputs for the current config.
func (m Model) libFindOptions() steamworks.FindOptions {
	return libFindOptions(m.cfg, m.paths)
}

func libFindOptions(cfg *config.Config, paths *steam.Paths) steamworks.FindOptions {
	opts := steamworks.FindOptions{Configured: cfg.SteamAPIPath, ConfigDir: cfg.Dir}
	if paths != nil {
		// The client's libraries, plus DayZ's own in case the client's
		// list does not have it.
		opts.SteamApps = append(steamworks.LibraryFolders(paths.ClientRoot()), paths.SteamApps)
	}
	return opts
}

// libKey identifies the inputs of a FindLibrary call.
func (m Model) libKey() string {
	root := ""
	if m.paths != nil {
		root = m.paths.ClientRoot() + "\x00" + m.paths.SteamApps
	}
	return strings.Join([]string{"lib", m.cfg.SteamAPIPath, m.cfg.Dir, root, strconv.Itoa(m.libGen)}, "\x00")
}

// libFoundMsg is a FindLibrary result computed off the UI goroutine.
type libFoundMsg libLookup

// findLibraryCmd looks up libsteam_api.so in the background.
func (m Model) findLibraryCmd() tea.Cmd {
	key, opts := m.libKey(), m.libFindOptions()
	return func() tea.Msg {
		path, err := steamworks.FindLibrary(opts)
		return libFoundMsg{key: key, path: path, err: err}
	}
}

// library returns the cached lookup. When the cache is stale it marks the
// lookup pending and returns the findLibraryCmd that redoes it; a libFoundMsg
// brings the result. A cached "not found" is also redone once a copy shows
// up where the README says to put one (~/.config/dzman/libsteam_api.so),
// which is one stat. While a lookup runs it returns errLibPending.
func (m *Model) library() (libLookup, tea.Cmd) {
	pending := libLookup{err: errLibPending, pending: true}
	if m.lib.pending {
		return pending, nil
	}
	if m.lib.key != m.libKey() || m.libAppeared() {
		m.lib = libLookup{pending: true}
		return pending, m.findLibraryCmd()
	}
	return m.lib, nil
}

// relookLibrary forces a fresh lookup even if no input changed.
func (m *Model) relookLibrary() { m.libGen++ }

// libAppeared reports whether the cached lookup found nothing but the
// config dir now has a libsteam_api.so.
func (m Model) libAppeared() bool {
	if !errors.Is(m.lib.err, steamworks.ErrLibraryNotFound) || m.cfg.Dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(m.cfg.Dir, steamworks.LibName))
	return err == nil
}

// backendInputs gathers the current state. It looks at the disk and /proc,
// so call it when building a page, not on every render. The cmd is a
// library lookup to run, if one is due.
func (m *Model) backendInputs() (backendInputs, tea.Cmd) {
	lib, cmd := m.library()
	return gatherBackendInputs(m.cfg, m.profile, m.paths, lib), cmd
}

// gatherBackendInputs is backendInputs for a given library lookup result.
func gatherBackendInputs(cfg *config.Config, profile *config.Profile, paths *steam.Paths, lib libLookup) backendInputs {
	in := backendInputs{
		mode:         cfg.Backend(),
		lib:          lib.path,
		libErr:       lib.err,
		steamRunning: launcher.IsSteamRunning(),
		flatpak:      paths != nil && paths.Flatpak(),
		cmdEnabled:   cfg.SteamCMDEnabled,
		loginOK:      profile.SteamLogin != "" && profile.SteamLogin != "anonymous",
	}
	in.cmdPath, in.cmdErr = resolveSteamCMD(cfg, profile, paths)
	return in
}

// resolveSteamCMD finds steamcmd and refuses one inside any Steam client
// root, known paths or not.
func (m Model) resolveSteamCMD() (string, error) {
	return resolveSteamCMD(m.cfg, m.profile, m.paths)
}

func resolveSteamCMD(cfg *config.Config, profile *config.Profile, paths *steam.Paths) (string, error) {
	extra := []string{profile.SteamRoot}
	if paths != nil {
		extra = append(extra, paths.Root(), paths.ClientRoot())
	}
	roots := steam.ClientRoots(extra...)
	path, err := steam.FindSteamCMD(cfg.SteamCMDPath, roots...)
	if err != nil {
		return "", err
	}
	// Auto-detect already skipped non-isolated copies; this is the check
	// that makes an explicitly configured path a hard error.
	if err := steam.CheckSteamCMDIsolated(path, roots...); err != nil {
		return "", err
	}
	return path, nil
}

// refreshBackend recomputes m.bk and m.backend. The caller must run the
// returned cmd (a background library lookup, or nil).
func (m *Model) refreshBackend() tea.Cmd {
	var cmd tea.Cmd
	m.bk, cmd = m.backendInputs()
	m.backend, m.backendWhy = resolveBackend(m.bk)
	return cmd
}

// backendLabel is e.g. "auto → steamworks", "steamcmd", or
// "steamcmd → manual" when the chosen mode cannot be used, or
// "auto → looking for Steam API library…" while that decides it.
func (m Model) backendLabel() string {
	mode := m.cfg.Backend()
	if mode == config.BackendAuto || mode != m.backend.String() {
		return mode + " → " + m.backend.String()
	}
	return mode
}

// nextBackend cycles through config.Backends, skipping steamcmd while
// SteamCMD is disabled. It never changes the enabled setting itself.
func nextBackend(cur string, cmdEnabled bool) string {
	i := -1
	for j, b := range config.Backends {
		if b == cur {
			i = j
		}
	}
	if i < 0 {
		return config.BackendAuto
	}
	for range config.Backends {
		i = (i + 1) % len(config.Backends)
		if b := config.Backends[i]; b != config.BackendSteamCMD || cmdEnabled {
			return b
		}
	}
	return config.BackendAuto
}

// steamAPIStatus describes the libsteam_api.so in use.
func (m Model) steamAPIStatus() string {
	if m.bk.libErr != nil {
		if errors.Is(m.bk.libErr, errLibPending) {
			return "looking…"
		}
		if errors.Is(m.bk.libErr, steamworks.ErrLibraryNotFound) {
			return "not found (see README)"
		}
		return "error: " + m.bk.libErr.Error()
	}
	return m.bk.lib
}

// ---- Steamworks download job -----------------------------------------

// dlPurpose is why a Steamworks job runs.
type dlPurpose int

const (
	dlInstall      dlPurpose = iota // missing mods for m.sel
	dlUpdate                        // Config > Update Mods
	dlUpdateServer                  // Server Options > Update Server Mods, for m.sel
)

// dlItem is one mod's progress in a Steamworks job.
type dlItem struct {
	name              string
	state             uint32
	downloaded, total uint64
	done              bool
	err               string
	skipped           string // why the helper left it alone
	// elsewhere is the install folder Steam reported when dzman does not
	// search it, so the mod will still look missing.
	elsewhere string
}

// dlState is a Steamworks job and its progress. Shared by pointer between
// Model copies.
type dlState struct {
	job       *steamworks.Job
	purpose   dlPurpose
	order     []string
	items     map[string]*dlItem
	cancelled bool
	exitErr   error
	scanErr   error // the rescan after the run failed
	exited    bool  // the helper process is gone
	released  bool  // and the Steam client let go of DayZ (or we gave up waiting)
	finished  bool  // and the follow-up scan is done
}

func newDLState(purpose dlPurpose, ids, names []string) *dlState {
	d := &dlState{purpose: purpose, items: map[string]*dlItem{}}
	for i, id := range ids {
		if _, dup := d.items[id]; dup {
			continue
		}
		d.order = append(d.order, id)
		it := &dlItem{name: names[i]}
		if !steam.ValidModID(id) {
			it.err = fmt.Sprintf("invalid workshop ID %q", id)
		}
		d.items[id] = it
	}
	return d
}

// validIDs are the items to hand the helper (not rejected up front).
func (d *dlState) validIDs() []string {
	var out []string
	for _, id := range d.order {
		if d.items[id].err == "" {
			out = append(out, id)
		}
	}
	return out
}

// skipped counts the items the helper skipped.
func (d *dlState) skipped() int {
	n := 0
	for _, it := range d.items {
		if it.skipped != "" {
			n++
		}
	}
	return n
}

// apply records one helper event.
func (d *dlState) apply(ev steamworks.Event) {
	it := d.items[ev.ID]
	if it == nil {
		return
	}
	it.state = ev.State
	switch ev.Type {
	case steamworks.EventProgress:
		it.downloaded, it.total = ev.Downloaded, ev.Total
	case steamworks.EventDone:
		it.done = true
		it.downloaded = it.total
	case steamworks.EventError:
		it.err = ev.Error
	case steamworks.EventSkipped:
		it.skipped = ev.Error
	}
}

// failures lists "name: error" for each failed item, and each item Steam
// installed somewhere dzman does not look.
func (d *dlState) failures() []string {
	var out []string
	for _, id := range d.order {
		switch it := d.items[id]; {
		case it.err != "":
			out = append(out, it.name+": "+it.err)
		case it.done && it.elsewhere != "":
			out = append(out, it.name+": installed to "+it.elsewhere+", which dzman doesn't search")
		}
	}
	return out
}

// Messages for the Steamworks job.
type (
	swStartedMsg struct {
		job *steamworks.Job
		err error
	}
	swEventMsg struct{ ev steamworks.Event }
	// swGoneMsg: the helper process exited. swExitMsg: and the Steam client
	// has had time to let go of DayZ.
	swGoneMsg struct{ err error }
	swExitMsg struct{ err error }
	// swRescanMsg is the mod scan taken after the helper exited.
	swRescanMsg struct {
		mods []steam.InstalledMod
		err  error
	}
)

// startSteamworksCmd re-execs dzman as the helper.
func startSteamworksCmd(opts steamworks.JobOptions) tea.Cmd {
	return func() tea.Msg {
		exe, err := steamworks.HelperExe()
		if err != nil {
			return swStartedMsg{err: err}
		}
		opts.Exe = exe
		job, err := steamworks.Start(opts)
		return swStartedMsg{job: job, err: err}
	}
}

// waitSteamworksCmd delivers the job's next event, or swGoneMsg once the
// helper has exited.
func waitSteamworksCmd(job *steamworks.Job) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-job.Events
		if ok {
			return swEventMsg{ev: ev}
		}
		return swGoneMsg{err: job.Wait()}
	}
}

// releaseSteamworksCmd waits for the client to let go of DayZ before
// anything can launch it (see waitSteamReleased), then sends swExitMsg.
func releaseSteamworksCmd(err error) tea.Cmd {
	return func() tea.Msg {
		waitSteamReleased(time.Sleep, time.Now, gameOrHelperRunning)
		return swExitMsg{err: err}
	}
}

// After the helper exits the Steam client still needs a moment to stop
// counting DayZ as running, and there is no way to ask it without the API.
// The floor is a guess (3s, up from 1s) that could not be checked against a
// real Steam client here. So wait at least releaseFloor, then until no DayZ or helper process is
// left (another dzman's helper, or a game someone started), for at most
// releaseMax. Where the client's registry.vdf says whether it counts DayZ
// as running, that is waited on too (see steam.AppRunning).
const (
	releaseFloor = 3 * time.Second
	releaseMax   = 10 * time.Second
	releasePoll  = 250 * time.Millisecond
)

func gameOrHelperRunning() bool {
	return launcher.IsDayZRunning() || steamworks.IsHelperRunning(procs.ByArgv) ||
		registrySaysRunning(steam.RegistryFiles())
}

// registrySaysRunning reports whether a registry file says the client still
// counts DayZ as running. No file or no key is not a yes.
func registrySaysRunning(files []string) bool {
	running, _ := steam.AppRunning(files, steam.GameID)
	return running
}

func waitSteamReleased(sleep func(time.Duration), now func() time.Time, busy func() bool) {
	start := now()
	sleep(releaseFloor)
	for busy() && now().Sub(start) < releaseMax {
		sleep(releasePoll)
	}
}

func rescanAfterSteamworksCmd(paths *steam.Paths) tea.Cmd {
	return func() tea.Msg {
		if paths == nil {
			return swRescanMsg{}
		}
		mods, err := steam.ScanAllMods(paths)
		return swRescanMsg{mods: mods, err: err}
	}
}

// searchesFolder reports whether a mod installed in folder is one dzman
// finds: folder is <one of ModDirs>/<ID>. Symlinks are resolved.
func (m Model) searchesFolder(folder string) bool { return searchesFolder(m.paths, folder) }

func searchesFolder(paths *steam.Paths, folder string) bool {
	if paths == nil {
		return false
	}
	parent := filepath.Dir(filepath.Clean(folder))
	rp := evalOr(parent)
	for _, d := range paths.ModDirs() {
		if d != "" && (filepath.Clean(d) == parent || evalOr(d) == rp) {
			return true
		}
	}
	return false
}

// evalOr is p with symlinks resolved, or p cleaned if that fails.
func evalOr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// updateBlockedWhy says why Config > Update Mods cannot run with the current
// backend, "" if it can.
func (m Model) updateBlockedWhy() string {
	switch m.backend {
	case backendPending:
		return "still " + pendingLabel + " Try again in a moment."
	case backendSteamworks:
		if ids, _ := m.clientMods(); len(ids) == 0 {
			return "no installed mods are in the Steam client's workshop folder"
		}
	case backendSteamCMD:
		if len(m.managedMods()) == 0 {
			return "dzman has not downloaded any mods with SteamCMD"
		}
	default:
		why := "the mod backend is manual, so dzman does not update mods; change it under Config > Mod Backend"
		if m.backendWhy != "" {
			why += "\n" + m.backendWhy
		}
		return why
	}
	if m.paths == nil {
		return "the DayZ folders are not set up yet"
	}
	return ""
}

// updateServerMods runs the backend's update for every mod of srv, installed
// or not. Steamworks subscribes the missing ones; SteamCMD only touches mods
// it manages or that are missing, so Steam's own copies are left alone.
func (m Model) updateServerMods(srv *api.Server) (Model, tea.Cmd) {
	if len(srv.Mods) == 0 {
		m.resultToServer = true
		return m.showModResult("Update Server Mods", dimStyle.Render("  This server has no mods."))
	}
	switch m.backend {
	case backendPending:
		return m.showUpdateError(errors.New(m.updateBlockedWhy()))
	case backendSteamworks:
		ids, names := modNames(srv.Mods)
		return m.startSteamworks(dlUpdateServer, ids, names)
	case backendSteamCMD:
		if m.paths == nil {
			return m.showUpdateError(errors.New(m.updateBlockedWhy()))
		}
		if !m.bk.steamCMDReady() {
			return m.showUpdateError(errors.New("SteamCMD: " + m.bk.steamCMDProblem()))
		}
		m.resultToServer = true // the result page returns to the server's options
		managed := map[string]bool{}
		for _, mod := range m.managedMods() {
			managed[mod.ID] = true
		}
		var mods []steam.InstalledMod
		seen := map[string]bool{}
		for _, mod := range srv.Mods {
			id := mod.SteamWorkshopID
			if seen[id] {
				continue
			}
			seen[id] = true
			if _, ok := m.paths.FindMod(id); !ok || managed[id] {
				mods = append(mods, steam.InstalledMod{ID: id, Name: mod.Name})
			}
		}
		if len(mods) == 0 {
			return m.showModResult("Update Mods", dimStyle.Render("  Nothing for SteamCMD to update: the server's mods are all Steam's own."))
		}
		m.loading = true
		m.loadingMsg = fmt.Sprintf("Updating %d mods with SteamCMD…", len(mods))
		m.pg = pageLoading
		return m, tea.Batch(m.spin.Tick, steamCMDUpdateCmd(m.bk.cmdPath, m.profile.SteamLogin,
			m.paths.CmdContent, mods))
	}
	return m.showUpdateError(errors.New(m.updateBlockedWhy()))
}

// showUpdateError shows why Update Mods could not start on the download page
// in its finished state, so esc returns to Config (or the server's options)
// instead of the error page (which quits).
func (m Model) showUpdateError(err error) (Model, tea.Cmd) {
	d := newDLState(dlUpdate, nil, nil)
	if m.pg == pageServerAction {
		d.purpose = dlUpdateServer
	}
	d.exited, d.finished, d.exitErr = true, true, err
	m.dl = d
	m.loading = false
	m.pg = pageModDownload
	return m, nil
}

// startSteamworks checks the preconditions and starts a job.
func (m Model) startSteamworks(purpose dlPurpose, ids, names []string) (Model, tea.Cmd) {
	fail := func(err error) (Model, tea.Cmd) {
		if purpose == dlInstall {
			m.swFailed = true
			return m.buildMissingModsNote(m.missingMods, redStyle.Render("Steamworks: "+err.Error()))
		}
		return m.showUpdateError(err)
	}
	if !launcher.IsSteamRunning() {
		return fail(errors.New("the Steam client is not running; start Steam, log in, and try again"))
	}
	if launcher.IsDayZRunning() {
		return fail(errors.New("DayZ is running; close it first"))
	}
	if steamworks.IsHelperRunning(procs.ByArgv) {
		return fail(errors.New("another Steamworks download is running (another dzman?); wait for it or cancel it"))
	}
	if m.bk.libErr != nil {
		return fail(m.bk.libErr)
	}
	d := newDLState(purpose, ids, names)
	valid := d.validIDs()
	if len(valid) == 0 {
		if purpose == dlInstall {
			return fail(errors.New("no valid workshop IDs: " + strings.Join(d.failures(), "; ")))
		}
		d.exited, d.finished = true, true
		m.dl = d
		m.pg = pageModDownload
		return m, nil
	}
	m.dl = d
	m.pg = pageModDownload
	m.loading = true
	return m, tea.Batch(m.spin.Tick, startSteamworksCmd(steamworks.JobOptions{
		Lib:       m.bk.lib,
		IDs:       valid,
		Subscribe: purpose != dlUpdate,
		Timeout:   m.cfg.SteamworksTimeout.Std(),
		Stall:     m.cfg.SteamworksStall.Std(),
	}))
}

// updateSteamworks handles the job messages.
func (m Model) updateSteamworks(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case swStartedMsg:
		if msg.err != nil {
			m.loading = false
			m.dl.finished, m.dl.exitErr = true, msg.err
			return m.finishSteamworks(nil)
		}
		m.dl.job = msg.job
		if m.dl.cancelled {
			msg.job.Cancel() // c was pressed before the helper started
		}
		return m, waitSteamworksCmd(msg.job)
	case swEventMsg:
		m.dl.apply(msg.ev)
		if ev := msg.ev; ev.Type == steamworks.EventDone && ev.Folder != "" && !m.searchesFolder(ev.Folder) {
			if it := m.dl.items[ev.ID]; it != nil {
				it.elsewhere = ev.Folder
			}
		}
		return m, waitSteamworksCmd(m.dl.job)
	case swGoneMsg:
		m.dl.job = nil
		m.dl.exited = true
		m.dl.exitErr = msg.err
		return m, releaseSteamworksCmd(msg.err)
	case swExitMsg:
		m.dl.released = true
		return m, rescanAfterSteamworksCmd(m.paths)
	case swRescanMsg:
		m.loading = false
		m.dl.finished = true
		if msg.err == nil {
			m.mods = msg.mods
		}
		m.dl.scanErr = msg.err
		return m.finishSteamworks(msg.err)
	}
	return m, nil
}

// finishSteamworks decides where to go once the helper has exited.
func (m Model) finishSteamworks(scanErr error) (Model, tea.Cmd) {
	d := m.dl
	if d.purpose != dlInstall {
		m.pg = pageModDownload // stay and show the result; esc goes back
		return m, nil
	}
	// A cancel comes first: whatever the helper said on its way out, the
	// user asked it to stop.
	m.swallowC = time.Now().Add(swallowCFor)
	missing := m.missingServerMods(m.sel)
	if d.cancelled {
		return m.buildMissingModsNote(missing, dimStyle.Render("Download cancelled. "+
			"Steam keeps any subscriptions and may finish the downloads on its own."))
	}
	if d.exitErr != nil || scanErr != nil {
		err := d.exitErr
		if err == nil {
			err = scanErr
		}
		m.swFailed = true
		return m.buildMissingModsNote(missing, redStyle.Render("Steamworks failed: "+err.Error()))
	}
	if len(missing) > 0 {
		m.swFailed = true
		note := fmt.Sprintf("%d mods still missing after the Steam download.", len(missing))
		if f := d.failures(); len(f) > 0 {
			note += "\n" + strings.Join(f, "\n")
		}
		return m.buildMissingModsNote(missing, redStyle.Render(note))
	}
	return m.promptPasswordOrLaunch(m.sel)
}

// swallowCFor is how long the missing-mods page ignores c after the download
// page hands over, so a c pressed to cancel is not read as "install with
// SteamCMD instead".
const swallowCFor = 500 * time.Millisecond

func (m Model) keyModDownload(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit // main cancels the helper
	case "c", "esc", "q":
		if m.dl == nil {
			return m, nil
		}
		if m.dl.exited && !m.dl.finished {
			return m, nil // the run is over; there is nothing left to cancel
		}
		if !m.dl.finished {
			if !m.dl.cancelled {
				m.dl.cancelled = true
				if m.dl.job != nil {
					m.dl.job.Cancel()
				}
			}
			return m, nil
		}
		// Only update results stay on this page once finished.
		purpose := m.dl.purpose
		m.dl = nil
		if purpose == dlUpdateServer {
			m.pg = pageServerAction
			return m.buildServerAction(m.sel, "", "", 0)
		}
		return m.buildConfigMenu()
	}
	return m, nil
}

func (m Model) viewModDownload() string {
	d := m.dl
	if d == nil {
		return ""
	}
	var sb strings.Builder
	title := "Downloading Mods with Steam"
	if d.purpose != dlInstall {
		title = "Updating Mods with Steam"
	}
	sb.WriteString(headerStyle.Render(fmt.Sprintf("%s (%d)", title, len(d.order))) + "\n\n")
	for _, id := range d.order {
		it := d.items[id]
		icon, status := m.spin.View(), "waiting"
		switch {
		case it.done && it.elsewhere != "":
			icon, status = redStyle.Render("!"), "installed to "+it.elsewhere+", which dzman doesn't search"
		case it.done:
			icon, status = greenStyle.Render("✓"), "installed"
		case it.skipped != "":
			icon, status = dimStyle.Render("–"), "skipped ("+it.skipped+")"
		case it.err != "":
			icon, status = redStyle.Render("✗"), it.err
		case it.state&steamworks.ItemDownloading != 0 || it.total > 0:
			status = "downloading"
			if it.total > 0 {
				status = fmt.Sprintf("%3d%%  %s / %s", min(it.downloaded*100/it.total, 100),
					formatBytes(int64(it.downloaded)), formatBytes(int64(it.total)))
			}
		case it.state&steamworks.ItemDownloadPending != 0:
			status = "queued in Steam"
		}
		if d.finished && !it.done && it.err == "" && it.skipped == "" {
			icon, status = redStyle.Render("✗"), "not finished"
			if d.cancelled {
				status = "cancelled"
			}
		}
		fmt.Fprintf(&sb, "  %s %-40s %s\n", icon, truncate(it.name, 40), dimStyle.Render(status))
	}
	sb.WriteString("\n")
	if n := d.skipped(); n > 0 {
		sb.WriteString(dimStyle.Render(fmt.Sprintf("  %d skipped: not subscribed in Steam, so Steam would not keep them.", n)) + "\n")
	}
	help := "c cancel  ctrl+c quit"
	switch {
	case d.finished:
		switch {
		case d.cancelled:
			sb.WriteString(dimStyle.Render("  Cancelled. Steam may finish queued downloads on its own.") + "\n")
		case d.exitErr != nil:
			sb.WriteString(redStyle.Render("  "+d.exitErr.Error()) + "\n")
		default:
			sb.WriteString(dimStyle.Render("  Done.") + "\n")
		}
		if d.scanErr != nil {
			sb.WriteString(redStyle.Render("  Could not rescan installed mods: "+d.scanErr.Error()) + "\n")
		}
		help = "esc back"
	case d.cancelled:
		sb.WriteString(dimStyle.Render("  Cancelling…") + "\n")
	case d.released:
		sb.WriteString(dimStyle.Render("  Checking installed mods…") + "\n")
	case d.exited:
		sb.WriteString(dimStyle.Render("  Closing the Steam connection…") + "\n")
	default:
		sb.WriteString(dimStyle.Render("  Steam is downloading into its normal workshop folder.\n"+
			"  DayZ shows as running in Steam until this finishes.") + "\n")
	}
	return lipgloss.JoinVertical(lipgloss.Left, sb.String(), helpStyle.Render("  "+help))
}

// ---- Shared mod helpers ----------------------------------------------

// modNames returns the IDs and display names of mods.
func modNames(mods []api.Mod) (ids, names []string) {
	for _, mod := range mods {
		ids = append(ids, mod.SteamWorkshopID)
		names = append(names, mod.Name)
	}
	return ids, names
}

// clientMods are installed mods in the Steam client's workshop dir.
func (m Model) clientMods() (ids, names []string) {
	if m.paths == nil {
		return nil, nil
	}
	for _, mod := range m.mods {
		if filepath.Dir(mod.Dir) == m.paths.Workshop {
			ids = append(ids, mod.ID)
			names = append(names, mod.Name)
		}
	}
	return ids, names
}

// managedMods are the dzman-managed mods in the steamcmd dir.
func (m Model) managedMods() []steam.InstalledMod {
	var out []steam.InstalledMod
	for _, mod := range m.mods {
		if mod.Managed {
			out = append(out, mod)
		}
	}
	return out
}

// showModResult reuses the viewport page for a short text result.
func (m Model) showModResult(title, body string) (Model, tea.Cmd) {
	m.loading = false
	m.pg = pageNews
	m.newsView = viewport.New(m.w-4, m.h-6)
	m.newsView.SetContent(titleStyle.Render(title) + "\n\n" + body + "\n")
	return m, nil
}
