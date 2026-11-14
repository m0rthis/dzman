package tui

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/m0rthis/dzman/internal/api"
	"github.com/m0rthis/dzman/internal/config"
	"github.com/m0rthis/dzman/internal/steam"
	"github.com/m0rthis/dzman/internal/steamworks"
)

func TestResolveBackend(t *testing.T) {
	noLib := errors.New("libsteam_api.so not found")
	noCmd := errors.New("steamcmd not found")
	tests := []struct {
		name    string
		in      backendInputs
		want    backendKind
		wantWhy bool
	}{
		{"auto, steam ready", backendInputs{mode: config.BackendAuto, steamRunning: true, cmdEnabled: true, loginOK: true}, backendSteamworks, false},
		{"auto, steam not running, steamcmd ready", backendInputs{mode: config.BackendAuto, cmdEnabled: true, loginOK: true}, backendSteamCMD, true},
		{"auto, no lib, steamcmd disabled", backendInputs{mode: config.BackendAuto, libErr: noLib, steamRunning: true, loginOK: true}, backendManual, true},
		{"auto, no lib, steamcmd missing", backendInputs{mode: config.BackendAuto, libErr: noLib, cmdEnabled: true, cmdErr: noCmd, loginOK: true}, backendManual, true},
		{"auto, no lib, anonymous login", backendInputs{mode: config.BackendAuto, libErr: noLib, cmdEnabled: true}, backendManual, true},
		{"empty mode is auto", backendInputs{steamRunning: true}, backendSteamworks, false},
		{"forced steamworks without steam", backendInputs{mode: config.BackendSteamworks, libErr: noLib}, backendSteamworks, false},
		{"forced steamcmd", backendInputs{mode: config.BackendSteamCMD, steamRunning: true, cmdEnabled: true}, backendSteamCMD, false},
		{"forced steamcmd but disabled", backendInputs{mode: config.BackendSteamCMD, steamRunning: true}, backendManual, true},
		{"forced manual", backendInputs{mode: config.BackendManual, steamRunning: true}, backendManual, false},
		// While the library lookup runs, steamcmd is not picked just because
		// the lookup has not answered yet.
		{"auto, lib pending", backendInputs{mode: config.BackendAuto, libErr: errLibPending, steamRunning: true, cmdEnabled: true, loginOK: true}, backendPending, false},
		{"auto, lib pending, steam not running", backendInputs{mode: config.BackendAuto, libErr: errLibPending, cmdEnabled: true, loginOK: true}, backendSteamCMD, true},
		{"auto, lib pending, flatpak", backendInputs{mode: config.BackendAuto, libErr: errLibPending, flatpak: true, steamRunning: true}, backendManual, true},
		{"forced steamworks, lib pending", backendInputs{mode: config.BackendSteamworks, libErr: errLibPending}, backendPending, false},
		{"forced steamcmd, lib pending", backendInputs{mode: config.BackendSteamCMD, libErr: errLibPending, cmdEnabled: true}, backendSteamCMD, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := resolveBackend(tt.in)
			if got != tt.want || (why != "") != tt.wantWhy {
				t.Errorf("got %s %q, want %s (why=%v)", got, why, tt.want, tt.wantWhy)
			}
		})
	}
}

func TestNextBackendCycles(t *testing.T) {
	cur := config.BackendAuto
	seen := map[string]bool{}
	for range config.Backends {
		seen[cur] = true
		cur = nextBackend(cur, true)
	}
	if cur != config.BackendAuto || len(seen) != len(config.Backends) {
		t.Errorf("cycle ended at %q, saw %v", cur, seen)
	}
	if nextBackend("junk", true) != config.BackendAuto {
		t.Error("unknown should reset to auto")
	}
	// Disabled: steamcmd is never offered.
	cur = config.BackendAuto
	for range config.Backends {
		cur = nextBackend(cur, false)
		if cur == config.BackendSteamCMD {
			t.Fatal("steamcmd offered while disabled")
		}
	}
}

func TestDLStateApply(t *testing.T) {
	d := newDLState(dlInstall, []string{"1", "2"}, []string{"A", "B"})
	d.apply(steamworks.Event{Type: steamworks.EventProgress, ID: "1", Downloaded: 5, Total: 10})
	d.apply(steamworks.Event{Type: steamworks.EventDone, ID: "1"})
	d.apply(steamworks.Event{Type: steamworks.EventError, ID: "2", Error: "stalled"})
	d.apply(steamworks.Event{Type: steamworks.EventDone, ID: "999"}) // unknown, ignored
	if it := d.items["1"]; !it.done || it.downloaded != 10 {
		t.Errorf("item 1 = %+v", it)
	}
	if f := d.failures(); len(f) != 1 || f[0] != "B: stalled" {
		t.Errorf("failures = %q", f)
	}
}

func TestDLStateInvalidAndSkipped(t *testing.T) {
	d := newDLState(dlUpdate, []string{"1", "-1", "2", "1e5"}, []string{"A", "Bad", "C", "Bad2"})
	if got := d.validIDs(); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("validIDs = %q", got)
	}
	if f := d.failures(); len(f) != 2 {
		t.Errorf("failures = %q", f)
	}
	d.apply(steamworks.Event{Type: steamworks.EventSkipped, ID: "2", Error: steamworks.NotSubscribedMsg})
	if d.skipped() != 1 || d.items["2"].err != "" {
		t.Errorf("skipped=%d item=%+v", d.skipped(), d.items["2"])
	}
}

func TestWaitSteamReleased(t *testing.T) {
	run := func(busyPolls int) time.Duration {
		clock := time.Unix(0, 0)
		start := clock
		polls := 0
		waitSteamReleased(func(d time.Duration) { clock = clock.Add(d) }, func() time.Time { return clock },
			func() bool { polls++; return polls <= busyPolls })
		return clock.Sub(start)
	}
	if got := run(0); got != releaseFloor {
		t.Errorf("idle: waited %s, want the floor %s", got, releaseFloor)
	}
	if got := run(4); got != releaseFloor+4*releasePoll {
		t.Errorf("busy 4 polls: waited %s", got)
	}
	if got := run(1 << 20); got < releaseMax || got > releaseMax+releasePoll {
		t.Errorf("always busy: waited %s, want about %s", got, releaseMax)
	}
}

// A late mod scan must not end a loading page that is waiting on something
// else (here a launch).
func TestModsScanDoesNotEndOtherLoading(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.loadedSrv, m.loadedNews, m.loadedMods = true, true, true
	m.pg, m.loading, m.loadingMsg = pageLoading, true, "Launching DayZ…"
	next, _ := m.Update(modsScannedMsg{})
	got := next.(Model)
	if got.pg != pageLoading || !got.loading {
		t.Errorf("pg=%v loading=%v; the launch's loading page was ended", got.pg, got.loading)
	}
}

// runLookup runs a library lookup cmd and feeds its libFoundMsg back.
func runLookup(t *testing.T, m Model, cmd func() tea.Msg) Model {
	t.Helper()
	msg, ok := cmd().(libFoundMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want libFoundMsg", msg)
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestLibraryLookupCached(t *testing.T) {
	m := New(&config.Config{SteamAPIPath: "/nonexistent/libsteam_api.so"}, &config.Profile{}, nil)
	// The startup lookup is pending: nothing is scanned inline.
	if got, cmd := m.library(); !errors.Is(got.err, errLibPending) || got.key != "" || cmd != nil {
		t.Fatalf("pending lookup = %+v cmd=%v", got, cmd != nil)
	}
	m = runLookup(t, m, m.findLibraryCmd())
	first, cmd := m.library()
	if first.err == nil || first.key == "" || first.pending || cmd != nil {
		t.Fatalf("lookup = %+v cmd=%v", first, cmd != nil)
	}
	// A cached result is reused as is.
	m.lib.err = nil
	m.lib.path = "/cached.so"
	if got, cmd := m.library(); got.path != "/cached.so" || cmd != nil {
		t.Errorf("cache not used: %+v", got)
	}
	// Changing an input invalidates it, and the lookup runs in the
	// background instead of inline.
	m.cfg.SteamAPIPath = "/other.so"
	got, cmd := m.library()
	if !errors.Is(got.err, errLibPending) || cmd == nil || !m.lib.pending {
		t.Fatalf("stale cache: %+v cmd=%v", got, cmd != nil)
	}
	m = runLookup(t, m, cmd)
	if got, _ := m.library(); got.pending || got.err == nil || !strings.Contains(got.err.Error(), "/other.so") {
		t.Errorf("after the background lookup: %+v", got)
	}
	// A background result for old inputs is ignored.
	next, _ := m.Update(libFoundMsg{key: "stale", path: "/stale.so"})
	if got := next.(Model).lib; got.path == "/stale.so" {
		t.Error("stale libFoundMsg applied")
	}
}

// Setting the Steam API path looks again in the background, even when the
// value did not change, and never scans on the UI goroutine.
func TestSetSteamAPIPathLooksUpAsync(t *testing.T) {
	m := New(&config.Config{ConfPath: filepath.Join(t.TempDir(), "dzman.json")}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m = runLookup(t, m, m.findLibraryCmd())
	before := m.lib.key
	m.inputs[0].SetValue("")
	m.step = setupSteamAPIPath
	got, cmd := m.advanceSetupSteamAPIPath()
	if !got.lib.pending || cmd == nil || got.pg != pageConfig {
		t.Fatalf("pending=%v cmd=%v pg=%v", got.lib.pending, cmd != nil, got.pg)
	}
	if !strings.Contains(got.steamAPIStatus(), "looking") {
		t.Errorf("status %q while pending", got.steamAPIStatus())
	}
	// The batch holds the lookup; find it and apply it.
	var found bool
	for _, c := range flattenBatch(cmd) {
		if msg, ok := c().(libFoundMsg); ok {
			found = true
			next, _ := got.Update(msg)
			got = next.(Model)
		}
	}
	if !found || got.lib.pending || got.lib.key == before {
		t.Errorf("found=%v lib=%+v", found, got.lib)
	}
}

// flattenBatch returns the cmds of a tea.Batch (or the cmd itself). Cmds
// that block (the spinner tick) are not in these batches.
func flattenBatch(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Cmd
		for _, c := range b {
			out = append(out, flattenBatch(c)...)
		}
		return out
	}
	return []tea.Cmd{func() tea.Msg { return msg }}
}

// A startup result for inputs that changed meanwhile triggers a fresh
// lookup instead of leaving the lookup pending forever.
func TestLibFoundStaleWhilePendingRelooks(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	next, cmd := m.Update(libFoundMsg{key: "stale"})
	if cmd == nil || !next.(Model).lib.pending {
		t.Errorf("cmd=%v pending=%v", cmd, next.(Model).lib.pending)
	}
	next, _ = m.Update(libFoundMsg{key: m.libKey(), err: steamworks.ErrLibraryNotFound})
	if got := next.(Model).lib; got.pending || !errors.Is(got.err, steamworks.ErrLibraryNotFound) {
		t.Errorf("lib = %+v", got)
	}
}

func TestNewDLStateDedupes(t *testing.T) {
	d := newDLState(dlInstall, []string{"2", "1", "2"}, []string{"B", "A", "B2"})
	if !reflect.DeepEqual(d.order, []string{"2", "1"}) || d.items["2"].name != "B" {
		t.Errorf("order=%q item=%+v", d.order, d.items["2"])
	}
}

func TestResolveBackendFlatpak(t *testing.T) {
	in := backendInputs{mode: config.BackendAuto, flatpak: true, steamRunning: true, cmdEnabled: true, loginOK: true}
	if got, why := resolveBackend(in); got != backendSteamCMD || !strings.Contains(why, "Flatpak") {
		t.Errorf("auto on Flatpak = %s %q, want steamcmd with a Flatpak reason", got, why)
	}
	in.mode = config.BackendSteamworks
	if got, _ := resolveBackend(in); got != backendSteamworks {
		t.Errorf("forced steamworks on Flatpak = %s", got)
	}
}

// A libsteam_api.so copied into the config dir after a "not found" lookup
// is picked up on the next page build without changing any setting.
func TestLibraryLookupRetriesNotFound(t *testing.T) {
	dir := t.TempDir()
	m := New(&config.Config{Dir: dir}, &config.Profile{}, nil)
	m = runLookup(t, m, m.findLibraryCmd())
	if got, cmd := m.library(); !errors.Is(got.err, steamworks.ErrLibraryNotFound) || cmd != nil {
		t.Fatalf("lookup = %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, steamworks.LibName), []byte("not an ELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, cmd := m.library()
	if !got.pending || cmd == nil {
		t.Fatalf("new copy not looked at: %+v", got)
	}
	m = runLookup(t, m, cmd)
	if got, _ := m.library(); got.err == nil || errors.Is(got.err, steamworks.ErrLibraryNotFound) {
		t.Errorf("new copy not looked at: %+v", got)
	}
}

// While the startup lookup runs, the backend is undecided: the missing-mods
// page says so and i does nothing. The lookup's result rebuilds the page.
// (steamworks mode, since auto also needs a running Steam client to stay
// undecided; resolveBackend's table covers auto.)
func TestMissingModsWhileLibraryPending(t *testing.T) {
	m := New(&config.Config{ModBackend: config.BackendSteamworks, SteamCMDEnabled: true}, &config.Profile{SteamLogin: "bob"}, nil)
	m.w, m.h = 100, 40
	missing := []api.Mod{{Name: "A", SteamWorkshopID: "1"}}
	m, _ = m.buildMissingModsNote(missing, "a note")
	if m.backend != backendPending || !strings.Contains(m.newsView.View(), "Looking for the Steam API library") ||
		!strings.Contains(m.backendLabel(), pendingLabel) {
		t.Fatalf("backend=%s page:\n%s", m.backend, m.newsView.View())
	}
	if !strings.Contains(m.viewMissingMods(), pendingLabel) {
		t.Errorf("help does not say it is looking:\n%s", m.viewMissingMods())
	}
	next, cmd := m.keyMissingMods(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if next.pg != pageMissingMods || cmd != nil {
		t.Errorf("i while pending: pg=%v cmd=%v", next.pg, cmd != nil)
	}
	upd, _ := m.Update(libFoundMsg{key: m.libKey(), err: steamworks.ErrLibraryNotFound})
	got := upd.(Model)
	if got.pg != pageMissingMods || got.backend != backendSteamworks {
		t.Fatalf("after lookup: pg=%v backend=%s", got.pg, got.backend)
	}
	if v := got.newsView.View(); strings.Contains(v, "Looking for the Steam API library") || !strings.Contains(v, "a note") {
		t.Errorf("page not rebuilt with its note:\n%s", v)
	}
}

// esc on the path prompts goes back to Config without saving.
func TestPathPromptEscCancels(t *testing.T) {
	for _, step := range []setupStep{setupSteamCMDPath, setupSteamAPIPath} {
		cfg := &config.Config{SteamCMDPath: "steamcmd", SteamAPIPath: "/keep.so", ConfPath: filepath.Join(t.TempDir(), "dzman.json")}
		m := New(cfg, &config.Profile{}, nil)
		m.w, m.h = 100, 40
		m.pg, m.step = pageSetup, step
		m.inputs[0].SetValue("/typed/but/cancelled")
		got, _ := m.keySetupInput(tea.KeyMsg{Type: tea.KeyEsc}, m.advanceSetupSteamAPIPath)
		if got.pg != pageConfig || cfg.SteamCMDPath != "steamcmd" || cfg.SteamAPIPath != "/keep.so" {
			t.Errorf("step %v: pg=%v cmd=%q api=%q", step, got.pg, cfg.SteamCMDPath, cfg.SteamAPIPath)
		}
		if _, err := os.Stat(cfg.ConfPath); err == nil {
			t.Errorf("step %v: config saved", step)
		}
	}
}

// A cancelled install never reads as "Steamworks failed", even when the
// helper's exit carried an error.
func TestFinishSteamworksCancelBeatsError(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m.dl = newDLState(dlInstall, []string{"1"}, []string{"A"})
	m.dl.cancelled, m.dl.exitErr = true, errors.New("steamworks helper: signal: killed")
	got, _ := m.finishSteamworks(nil)
	if got.swFailed || got.pg != pageMissingMods {
		t.Errorf("swFailed=%v pg=%v", got.swFailed, got.pg)
	}
	if v := got.newsView.View(); strings.Contains(v, "failed") || !strings.Contains(v, "cancelled") {
		t.Errorf("page:\n%s", v)
	}
}

// A done event whose folder dzman does not search marks the item, through
// the real swEventMsg handling and searchesFolder.
func TestSteamworksEventElsewhere(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.paths = &steam.Paths{SteamApps: "/s/steamapps", Workshop: "/s/steamapps/workshop/content/221100", CmdWorkshop: "/d/ws"}
	m.dl = newDLState(dlInstall, []string{"1", "2"}, []string{"A", "B"})
	m.dl.job = &steamworks.Job{}
	for _, ev := range []steamworks.Event{
		{Type: steamworks.EventDone, ID: "1", Folder: "/mnt/other/steamapps/workshop/content/221100/1"},
		{Type: steamworks.EventDone, ID: "2", Folder: "/s/steamapps/workshop/content/221100/2"},
	} {
		next, cmd := m.updateSteamworks(swEventMsg{ev: ev})
		if cmd == nil {
			t.Fatal("no wait for the next event")
		}
		m = next
	}
	if it := m.dl.items["1"]; !it.done || it.elsewhere != "/mnt/other/steamapps/workshop/content/221100/1" {
		t.Errorf("item 1 = %+v", it)
	}
	if it := m.dl.items["2"]; !it.done || it.elsewhere != "" {
		t.Errorf("item 2 = %+v", it)
	}
	if f := m.dl.failures(); len(f) != 1 || !strings.Contains(f[0], "A: installed to /mnt/other/") ||
		!strings.Contains(f[0], "which dzman doesn't search") {
		t.Errorf("failures = %q", f)
	}
}

func TestSearchesFolder(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.paths = &steam.Paths{SteamApps: "/s/steamapps", Workshop: "/s/steamapps/workshop/content/221100", CmdWorkshop: "/d/ws"}
	for folder, want := range map[string]bool{
		"/s/steamapps/workshop/content/221100/1":       true,
		"/d/ws/2":                                      true,
		"/mnt/lib/steamapps/workshop/content/221100/3": false,
	} {
		if got := m.searchesFolder(folder); got != want {
			t.Errorf("searchesFolder(%s) = %v, want %v", folder, got, want)
		}
	}
}

// A mod scan landing on the config menu keeps the cursor on its entry.
func TestConfigMenuRescanKeepsCursor(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m, _ = m.buildConfigMenu()
	about := -1
	for i, it := range m.list.Items() {
		if it.(menuItem).label == "About" {
			about = i
		}
	}
	m.list.Select(about)
	// The scan adds "Remove All Mod Links" above About.
	next, _ := m.Update(modsScannedMsg{mods: []steam.InstalledMod{{ID: "1", Name: "A", Dir: "/x/1"}}})
	got := next.(Model)
	if it, ok := got.list.SelectedItem().(menuItem); !ok || it.label != "About" {
		t.Errorf("cursor on %+v, want About", got.list.SelectedItem())
	}
}

// doLaunch refuses a malformed mod ID even when no Steam paths are known.
func TestDoLaunchRefusesInvalidModIDs(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	srv := &api.Server{Mods: []api.Mod{{Name: "Evil", SteamWorkshopID: "../../x"}}}
	got, cmd := m.doLaunch(srv, "")
	if got.pg != pageError || got.err == nil || cmd != nil {
		t.Errorf("pg=%v err=%v cmd=%v", got.pg, got.err, cmd != nil)
	}
}

// Once the helper has exited, c/esc/q do nothing: the run is over and must
// not be reported as cancelled.
func TestModDownloadKeysIgnoredAfterExit(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.dl = newDLState(dlInstall, []string{"1"}, []string{"A"})
	m.dl.exited = true
	for _, k := range []string{"c", "esc", "q"} {
		got, _ := m.keyModDownload(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if k == "esc" {
			got, _ = m.keyModDownload(tea.KeyMsg{Type: tea.KeyEsc})
		}
		if got.dl.cancelled {
			t.Errorf("key %q marked a finished run cancelled", k)
		}
	}
}

// An Update Mods precondition failure stays on the download page, where esc
// goes back to Config instead of quitting.
func TestUpdateErrorEscReturnsToConfig(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	got, _ := m.showUpdateError(errors.New("boom"))
	if got.pg != pageModDownload || !strings.Contains(got.viewModDownload(), "boom") {
		t.Fatalf("pg=%v", got.pg)
	}
	got, _ = got.keyModDownload(tea.KeyMsg{Type: tea.KeyEsc})
	if got.pg != pageConfig || got.dl != nil {
		t.Errorf("pg=%v dl=%v", got.pg, got.dl)
	}
}

// c right after the download page hands over is swallowed.
func TestMissingModsSwallowsCAfterDownload(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m.dl = newDLState(dlInstall, []string{"1"}, []string{"A"})
	m.dl.cancelled = true
	got, _ := m.finishSteamworks(nil)
	if !time.Now().Before(got.swallowC) {
		t.Error("c not swallowed after finishing")
	}
}

// Update Mods is always offered; with the manual backend selecting it
// explains why instead of doing nothing.
func TestConfigUpdateModsExplainsWhenBlocked(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m.backend = backendManual
	got, _ := m.handleConfigSelect("Update Mods")
	if got.pg != pageModDownload || !strings.Contains(got.viewModDownload(), "manual") {
		t.Fatalf("pg=%v", got.pg)
	}
}

// A server's update error returns to its options page on esc.
func TestServerUpdateErrorEscReturnsToServer(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m.backend = backendManual
	m.sel = &api.Server{Name: "s", Mods: []api.Mod{{SteamWorkshopID: "1"}}}
	m.pg = pageServerAction
	got, _ := m.updateServerMods(m.sel)
	if got.pg != pageModDownload || !strings.Contains(got.viewModDownload(), "manual") {
		t.Fatalf("pg=%v", got.pg)
	}
	got, _ = got.keyModDownload(tea.KeyMsg{Type: tea.KeyEsc})
	if got.pg != pageServerAction || got.dl != nil {
		t.Errorf("pg=%v dl=%v", got.pg, got.dl)
	}
}

// A server with no mods says so and esc returns to the server's options with
// the cursor on Update Server Mods.
func TestServerUpdateNoMods(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m.backend = backendSteamworks
	m.sel = &api.Server{Name: "s"}
	m.pg = pageServerAction
	got, _ := m.updateServerMods(m.sel)
	if got.pg != pageNews || !strings.Contains(got.newsView.View(), "no mods") {
		t.Fatalf("pg=%v", got.pg)
	}
	got, _ = got.keyNews(tea.KeyMsg{Type: tea.KeyEsc})
	if got.pg != pageServerAction || got.resultToServer {
		t.Fatalf("pg=%v", got.pg)
	}
	if got.list.Index() != 1 {
		t.Errorf("cursor=%d", got.list.Index())
	}
}

// A SteamCMD update started from a server returns there; Config's does not.
func TestSteamCMDResultReturnsToOrigin(t *testing.T) {
	m := New(&config.Config{}, &config.Profile{}, nil)
	m.w, m.h = 100, 40
	m.sel = &api.Server{Name: "s"}
	m.pg = pageServerAction
	m.resultToServer = true
	got, _ := m.Update(steamCMDUpdatedMsg{n: 1})
	mm := got.(Model)
	if mm.pg != pageNews {
		t.Fatalf("pg=%v", mm.pg)
	}
	mm, _ = mm.keyNews(tea.KeyMsg{Type: tea.KeyEsc})
	if mm.pg != pageServerAction {
		t.Errorf("server: pg=%v", mm.pg)
	}

	m.resultToServer = false
	got, _ = m.Update(steamCMDUpdatedMsg{n: 1})
	mm = got.(Model)
	mm, _ = mm.keyNews(tea.KeyMsg{Type: tea.KeyEsc})
	if mm.pg != pageMain {
		t.Errorf("config: pg=%v", mm.pg)
	}
}
