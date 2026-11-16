package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/m0rthis/dzman/internal/api"
	"github.com/m0rthis/dzman/internal/config"
	"github.com/m0rthis/dzman/internal/steam"
)

func TestRejoinTarget(t *testing.T) {
	if _, err := RejoinTarget(&config.Profile{}); !errors.Is(err, ErrNoHistory) {
		t.Errorf("empty history: err = %v, want ErrNoHistory", err)
	}
	p := &config.Profile{}
	p.AddToHistory("Old", "1.1.1.1", 2303, 10)
	p.AddToHistory("New", "2.2.2.2", 2403, 10)
	got, err := RejoinTarget(p)
	if err != nil {
		t.Fatal(err)
	}
	want := JoinTarget{IP: "2.2.2.2", Port: 2403, Name: "New"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFindServer(t *testing.T) {
	list := []api.Server{
		{Name: "a", Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2305}, GamePort: 2302},
		{Name: "b", Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2405}, GamePort: 2402},
	}
	tests := []struct {
		t    JoinTarget
		want string
	}{
		{JoinTarget{IP: "1.1.1.1", Port: 2405}, "b"},                   // query port
		{JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true}, "a"}, // game port
		{JoinTarget{IP: "1.1.1.1", Port: 2302}, ""},                    // game port is not a query port
		{JoinTarget{IP: "9.9.9.9", Port: 2405}, ""},
	}
	for _, tt := range tests {
		got := findServer(list, tt.t)
		switch {
		case tt.want == "" && got != nil, tt.want != "" && (got == nil || got.Name != tt.want):
			t.Errorf("findServer(%+v) = %v, want %q", tt.t, got, tt.want)
		}
	}
}

func TestMissingMods(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "111"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := &steam.Paths{Workshop: ws}
	srv := &api.Server{Mods: []api.Mod{
		{Name: "have", SteamWorkshopID: "111"},
		{Name: "lack", SteamWorkshopID: "222"},
	}}
	got := missingMods(paths, srv)
	if len(got) != 1 || got[0].Name != "lack" {
		t.Errorf("missingMods = %v, want only lack", got)
	}
	if missingMods(nil, srv) != nil || missingMods(paths, nil) != nil {
		t.Error("nil paths or server must report nothing missing")
	}
}

func TestPrepareLaunch(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ProfilePath: filepath.Join(dir, "profile.json"), HistorySize: 5}
	profile := config.DefaultProfile()
	profile.Player = "me"
	srv := &api.Server{
		Name:     "Srv",
		Endpoint: api.Endpoint{IP: "3.3.3.3", Port: 2305},
		GamePort: 2302,
		Mods:     []api.Mod{{SteamWorkshopID: "111"}, {SteamWorkshopID: "222"}},
	}
	args, err := prepareLaunch(cfg, profile, nil, srv, "pw")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-mod=@111;@222", "-connect=3.3.3.3", "-port=2302", "-password=pw"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}
	if len(profile.History) != 0 {
		t.Errorf("prepareLaunch must not record history, got %+v", profile.History)
	}

	bad := &api.Server{Name: "Bad", Mods: []api.Mod{{SteamWorkshopID: "../x"}}}
	if _, err := prepareLaunch(cfg, profile, nil, bad, ""); err == nil {
		t.Error("invalid workshop ID must fail")
	}

	recordJoin(cfg, profile, srv)
	if len(profile.History) != 1 || profile.History[0].Name != "Srv" || profile.History[0].Port != 2305 {
		t.Errorf("history = %+v", profile.History)
	}
	if _, err := os.Stat(cfg.ProfilePath); err != nil {
		t.Errorf("profile was not saved: %v", err)
	}
}

func listed(srv api.Server) func(context.Context, *config.Config, bool) (serverListResult, error) {
	return func(context.Context, *config.Config, bool) (serverListResult, error) {
		return serverListResult{list: &api.ServerList{Result: []api.Server{srv}}}, nil
	}
}

// joinFixture runs join against a listed mod-less server with startDayZ
// replaced by launchErr, and returns the profile, the save path and the error.
func joinFixture(t *testing.T, ctx context.Context, launchErr error) (*config.Profile, string, error) {
	t.Helper()
	stubLoader(t, listed(api.Server{Name: "Srv", Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2305}, GamePort: 2302}))
	old := startDayZ
	startDayZ = func(_ string, _ []string, started func()) error {
		if launchErr == nil && started != nil {
			started()
		}
		return launchErr
	}
	t.Cleanup(func() { startDayZ = old })
	dir := t.TempDir()
	cfg := &config.Config{ProfilePath: filepath.Join(dir, "profile.json"), HistorySize: 5}
	profile := config.DefaultProfile()
	err := join(ctx, func() {}, JoinOptions{
		Cfg: cfg, Profile: profile, Out: &bytes.Buffer{},
		Target: JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true},
	})
	return profile, cfg.ProfilePath, err
}

func TestJoinRecordsHistoryOnlyAfterLaunch(t *testing.T) {
	profile, path, err := joinFixture(t, context.Background(), nil)
	if err != nil || len(profile.History) != 1 {
		t.Fatalf("err = %v, history = %+v", err, profile.History)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("profile not saved: %v", err)
	}

	profile, path, err = joinFixture(t, context.Background(), errors.New("steam missing"))
	if err == nil || len(profile.History) != 0 {
		t.Errorf("failed launch: err = %v, history = %+v", err, profile.History)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("failed launch saved the profile")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	profile, _, err = joinFixture(t, ctx, nil)
	if !errors.Is(err, ErrInterrupted) || len(profile.History) != 0 {
		t.Errorf("interrupted: err = %v, history = %+v", err, profile.History)
	}
}

func TestJoinKeepsHistoryOnceSteamStarted(t *testing.T) {
	stubLoader(t, listed(api.Server{Name: "Srv", Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2305}, GamePort: 2302}))
	old := startDayZ
	// steam was started, then the wait for the game ended in an error.
	startDayZ = func(_ string, _ []string, started func()) error {
		started()
		return errors.New("interrupted while waiting")
	}
	t.Cleanup(func() { startDayZ = old })
	cfg := &config.Config{ProfilePath: filepath.Join(t.TempDir(), "profile.json"), HistorySize: 5}
	profile := config.DefaultProfile()
	_ = join(context.Background(), func() {}, JoinOptions{
		Cfg: cfg, Profile: profile, Out: &bytes.Buffer{},
		Target: JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true},
	})
	if len(profile.History) != 1 {
		t.Errorf("history = %+v, want the join kept", profile.History)
	}
}

func TestJoinInterruptedWhileLoadingList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stubLoader(t, func(context.Context, *config.Config, bool) (serverListResult, error) {
		cancel() // the signal arrives during the load, which then fails
		return serverListResult{}, context.Canceled
	})
	var out bytes.Buffer
	err := join(ctx, func() {}, JoinOptions{
		Cfg: &config.Config{}, Profile: &config.Profile{}, Out: &out,
		Target: JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true},
	})
	if !errors.Is(err, ErrInterrupted) || out.Len() != 0 {
		t.Errorf("err = %v, out = %q", err, out.String())
	}
}

func TestRejoinNotListedKeepsTUI(t *testing.T) {
	p := &config.Profile{}
	p.AddToHistory("Gone\x1b[2J", "1.1.1.1", 2305, 10)
	m := New(&config.Config{}, p, nil)
	m.pg = pageMain
	m.loadedSrv = true
	m, cmd := m.rejoin()
	if m.pg != pageMain || cmd != nil || m.err != nil {
		t.Fatalf("page %v, err %v", m.pg, m.err)
	}
	if !strings.Contains(m.mainNote, "isn't in the server list") || strings.ContainsRune(m.mainNote, 0x1b) {
		t.Errorf("note = %q", m.mainNote)
	}
	m, _ = m.keyMain(tea.KeyMsg{Type: tea.KeyDown})
	if m.mainNote != "" {
		t.Error("the note must clear on the next key")
	}
}

func TestTUILaunchRecordsWhenSteamStarted(t *testing.T) {
	cfg := &config.Config{ProfilePath: filepath.Join(t.TempDir(), "profile.json"), HistorySize: 5}
	p := config.DefaultProfile()
	m := New(cfg, p, nil)
	srv := &api.Server{Name: "Srv", Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2305}}

	release := make(chan struct{})
	old := startDayZ
	startDayZ = func(_ string, _ []string, started func()) error {
		started()
		<-release // the wait for the game to appear
		return nil
	}
	t.Cleanup(func() { startDayZ = old })

	msg := launchServerCmd(srv, "me", nil)()
	st, ok := msg.(gameStartedMsg)
	if !ok {
		t.Fatalf("first message is %T, want gameStartedMsg", msg)
	}
	if len(p.History) != 0 {
		t.Fatal("the command goroutine must not touch the profile")
	}
	_, cmd := m.Update(st)
	if len(p.History) != 1 || p.History[0].Name != "Srv" {
		t.Errorf("history = %+v, want the join recorded on the model thread", p.History)
	}
	close(release)
	if _, ok := cmd().(gameLaunchedMsg); !ok {
		t.Error("the wait command must deliver gameLaunchedMsg")
	}
}

func TestTUILaunchStartedThenImmediateReturn(t *testing.T) {
	old := startDayZ
	startDayZ = func(_ string, _ []string, started func()) error {
		started()
		return errors.New("returned at once")
	}
	t.Cleanup(func() { startDayZ = old })
	for range 200 {
		msg := launchServerCmd(&api.Server{Name: "Srv"}, "me", nil)()
		st, ok := msg.(gameStartedMsg)
		if !ok {
			t.Fatalf("got %T, want gameStartedMsg so the join is recorded", msg)
		}
		if lm := st.wait().(gameLaunchedMsg); lm.err == nil {
			t.Fatal("the wait must hand back the launch error")
		}
	}
}

func TestTUILaunchCmdDoesNotLaunchUntilRun(t *testing.T) {
	launched := false
	old := startDayZ
	startDayZ = func(string, []string, func()) error { launched = true; return nil }
	t.Cleanup(func() { startDayZ = old })
	_ = launchServerCmd(&api.Server{Name: "Srv"}, "me", nil)
	if launched {
		t.Error("building the command launched the game")
	}
}

func TestTUILaunchFailureRecordsNothing(t *testing.T) {
	cfg := &config.Config{ProfilePath: filepath.Join(t.TempDir(), "profile.json"), HistorySize: 5}
	p := config.DefaultProfile()
	m := New(cfg, p, nil)
	old := startDayZ
	startDayZ = func(string, []string, func()) error { return errors.New("no steam") }
	t.Cleanup(func() { startDayZ = old })

	msg := launchServerCmd(&api.Server{Name: "Srv"}, "me", nil)()
	lm, ok := msg.(gameLaunchedMsg)
	if !ok || lm.err == nil {
		t.Fatalf("got %#v", msg)
	}
	m.Update(lm)
	if len(p.History) != 0 {
		t.Errorf("history = %+v", p.History)
	}
}

func TestRejoinNotes(t *testing.T) {
	p := &config.Profile{}
	p.AddToHistory("S", "1.1.1.1", 2305, 10)
	m := New(&config.Config{}, p, nil)
	m, _ = m.rejoin()
	if !strings.Contains(m.mainNote, "still loading") {
		t.Errorf("note = %q", m.mainNote)
	}
	m.loadedSrv, m.srvErr = true, errors.New("offline")
	m, _ = m.rejoin()
	if !strings.Contains(m.mainNote, "failed to load") {
		t.Errorf("note = %q", m.mainNote)
	}
	m.srvErr = nil
	m, _ = m.rejoin()
	if !strings.Contains(m.mainNote, "isn't in the server list") {
		t.Errorf("note = %q", m.mainNote)
	}
}

func TestLookupServerRefetchError(t *testing.T) {
	stubLoader(t, func(_ context.Context, _ *config.Config, force bool) (serverListResult, error) {
		if force {
			return serverListResult{}, errors.New("offline")
		}
		return serverListResult{list: &api.ServerList{}}, nil
	})
	srv, err := lookupServer(context.Background(), &bytes.Buffer{}, &config.Config{}, JoinTarget{IP: "1.1.1.1", Port: 1})
	if srv != nil || err == nil || !strings.Contains(err.Error(), "refreshing it failed") {
		t.Errorf("got %v, %v", srv, err)
	}
	// rejoin reports it; connect falls back.
	err = join(context.Background(), func() {}, JoinOptions{
		Cfg: &config.Config{}, Profile: &config.Profile{}, Out: &bytes.Buffer{},
		Target: JoinTarget{IP: "1.1.1.1", Port: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "refreshing it failed") {
		t.Errorf("rejoin err = %v", err)
	}
}

func stubLoader(t *testing.T, f func(context.Context, *config.Config, bool) (serverListResult, error)) {
	t.Helper()
	old := serverLoader
	serverLoader = f
	t.Cleanup(func() { serverLoader = old })
}

func TestLookupServerRetriesFresh(t *testing.T) {
	srv := api.Server{Name: "new", Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2305}, GamePort: 2302}
	var forced []bool
	stubLoader(t, func(_ context.Context, _ *config.Config, force bool) (serverListResult, error) {
		forced = append(forced, force)
		if force {
			return serverListResult{list: &api.ServerList{Result: []api.Server{srv}}, fetched: true}, nil
		}
		return serverListResult{list: &api.ServerList{}}, nil
	})
	got, err := lookupServer(context.Background(), &bytes.Buffer{}, &config.Config{}, JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true})
	if err != nil || got == nil || got.Name != "new" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(forced) != 2 || forced[0] || !forced[1] {
		t.Errorf("loads = %v, want cached then forced", forced)
	}
}

func TestLookupServerNoRefetchWhenFresh(t *testing.T) {
	for _, res := range []serverListResult{
		{list: &api.ServerList{}, fetched: true},
		{list: &api.ServerList{}, stale: true},
	} {
		calls := 0
		stubLoader(t, func(context.Context, *config.Config, bool) (serverListResult, error) {
			calls++
			return res, nil
		})
		var out bytes.Buffer
		got, err := lookupServer(context.Background(), &out, &config.Config{}, JoinTarget{IP: "1.1.1.1", Port: 1})
		if got != nil || err != nil || calls != 1 {
			t.Errorf("%+v: got %v, %v after %d loads", res, got, err, calls)
		}
		if res.stale != strings.Contains(out.String(), "out of date") {
			t.Errorf("%+v: note = %q", res, out.String())
		}
	}
}

func TestLoadServersForJoinStaleFallback(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "servers.json")
	sl := &api.ServerList{Result: []api.Server{{Name: "cached"}}}
	if err := api.SaveCache(cache, sl); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cache, old, old); err != nil {
		t.Fatal(err)
	}
	// Nothing listens on port 1, so the fetch fails.
	cfg := &config.Config{ServersCachePath: cache, APIBaseURL: "http://127.0.0.1:1", ServerDBTTL: config.Duration(time.Hour)}
	res, err := loadServersForJoin(context.Background(), cfg, false)
	if err != nil || !res.stale || len(res.list.Result) != 1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	cfg.ServersCachePath = filepath.Join(dir, "none.json")
	if _, err := loadServersForJoin(context.Background(), cfg, false); err == nil {
		t.Error("no cache and no network must fail")
	}
}

func TestSanitizeText(t *testing.T) {
	got := SanitizeText("na\x1b]52;c;ZXZpbA==\x07me\r\n\u009b2J ok")
	if strings.ContainsFunc(got, unicode.IsControl) || !strings.HasPrefix(got, "na") || !strings.HasSuffix(got, " ok") {
		t.Errorf("got %q", got)
	}
}

func TestConnectFallsBackWithoutList(t *testing.T) {
	stubLoader(t, func(context.Context, *config.Config, bool) (serverListResult, error) {
		return serverListResult{}, errors.New("offline")
	})
	var out bytes.Buffer
	o := JoinOptions{Cfg: &config.Config{}, Profile: &config.Profile{}, Out: &out,
		Target: JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true}}
	launched := false
	oldStart := startDayZ
	startDayZ = func(string, []string, func()) error { launched = true; return nil }
	t.Cleanup(func() { startDayZ = oldStart })
	err := join(context.Background(), func() {}, o)
	if err != nil || !launched || !strings.Contains(out.String(), "launching without mods") {
		t.Errorf("err = %v, launched = %v, out = %q", err, launched, out.String())
	}

	// rejoin needs the list.
	o.Target.ByGamePort = false
	err = join(context.Background(), func() {}, o)
	if err == nil || !strings.Contains(err.Error(), "load server list") || !strings.Contains(err.Error(), "rejoin needs") {
		t.Errorf("rejoin err = %v", err)
	}
}

func TestJoinModsNeedSetup(t *testing.T) {
	stubLoader(t, listed(api.Server{
		Endpoint: api.Endpoint{IP: "1.1.1.1", Port: 2305}, GamePort: 2302,
		Mods: []api.Mod{{SteamWorkshopID: "111"}},
	}))
	err := join(context.Background(), func() {}, JoinOptions{
		Cfg: &config.Config{}, Profile: &config.Profile{}, Out: &bytes.Buffer{},
		Target: JoinTarget{IP: "1.1.1.1", Port: 2302, ByGamePort: true},
	})
	if !errors.Is(err, errNotSetUp) {
		t.Errorf("err = %v, want errNotSetUp", err)
	}
}

func TestInstallMissingModsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := installMissingMods(ctx, "/nonexistent", "u", &steam.Paths{}, []api.Mod{{SteamWorkshopID: "111"}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestMainListRejoinItem(t *testing.T) {
	first := func(p *config.Profile) string {
		m := New(&config.Config{}, p, nil)
		m, _ = m.buildMainList()
		return m.list.Items()[0].(menuItem).label
	}
	if got := first(&config.Profile{}); got != "All Servers" {
		t.Errorf("empty history: first item %q", got)
	}
	p := &config.Profile{}
	p.AddToHistory("Cool Server", "1.1.1.1", 2305, 10)
	if got := first(p); got != "Rejoin Cool Server" {
		t.Errorf("first item %q, want the rejoin item", got)
	}

	// The cursor is on All Servers however the menu is built, at startup and
	// when returning to it, so Enter does not join by accident.
	p.Player = "me"
	m := New(&config.Config{}, p, nil)
	m.loadedSrv, m.loadedNews, m.loadedMods = true, true, true
	m, _ = m.maybeFinishLoading()
	if got := m.list.SelectedItem().(menuItem).label; got != "All Servers" {
		t.Errorf("startup cursor on %q, want All Servers", got)
	}
	m, _ = m.buildMainList()
	if got := m.list.SelectedItem().(menuItem).label; got != "All Servers" {
		t.Errorf("rebuilt cursor on %q, want All Servers", got)
	}
}

// An update hands every mod of the server to the backend; a plain join only
// the missing ones.
func TestModsToSync(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "111"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := &steam.Paths{Workshop: ws}
	srv := &api.Server{Mods: []api.Mod{
		{Name: "have", SteamWorkshopID: "111"},
		{Name: "lack", SteamWorkshopID: "222"},
	}}
	if got := modsToSync(paths, srv, false); len(got) != 1 || got[0].Name != "lack" {
		t.Errorf("join syncs %v, want only lack", got)
	}
	if got := modsToSync(paths, srv, true); len(got) != 2 {
		t.Errorf("update syncs %v, want both mods", got)
	}
	if got := modsToSync(nil, srv, true); got != nil {
		t.Errorf("without paths update syncs %v, want nothing", got)
	}
}

// steamcmd re-downloads what it installed itself and what is missing, and
// leaves the Steam client's own mods alone.
func TestSteamCMDUpdatable(t *testing.T) {
	ws := t.TempDir()
	for _, id := range []string{"111", "222"} {
		if err := os.Mkdir(filepath.Join(ws, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	paths := &steam.Paths{Workshop: ws}
	installed := []steam.InstalledMod{{ID: "111"}, {ID: "222", Managed: true}}
	mods := []api.Mod{
		{Name: "steam's", SteamWorkshopID: "111"},
		{Name: "managed", SteamWorkshopID: "222"},
		{Name: "missing", SteamWorkshopID: "333"},
	}
	got := steamCMDUpdatable(paths, installed, mods)
	if len(got) != 2 || got[0].Name != "managed" || got[1].Name != "missing" {
		t.Errorf("steamCMDUpdatable = %v, want managed and missing", got)
	}
}

func TestServerSummary(t *testing.T) {
	full := &api.Server{
		Map: "chernarus\x1b[2Jplus", Players: 23, MaxPlayers: 60, Time: "21:40", TimeAcceleration: 4,
		FirstPersonOnly: true, Version: "1.28.1", Mods: []api.Mod{{}, {}},
	}
	want := "map chernarus[2Jplus · players 23/60 · time 21:40 (night, x4) · first person only · version 1.28.1 · 2 mods"
	if got := serverSummary(full); got != want {
		t.Errorf("full:\n got %q\nwant %q", got, want)
	}
	if got := serverSummary(&api.Server{Time: "09:05"}); got != "time 09:05 (day)" {
		t.Errorf("time only: got %q", got)
	}
	if got := serverSummary(&api.Server{}); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

func TestGameHour(t *testing.T) {
	for in, want := range map[string]bool{"08:30": true, "09:05": true, "9:05": true, "18:59": true, "05:59": false, "19:00": false, "0:10": false, "": true} {
		if got := isDay(in); got != want {
			t.Errorf("isDay(%q) = %v, want %v", in, got, want)
		}
	}
	if got := timeEmoji("09:05"); got != "☀" {
		t.Errorf("timeEmoji(09:05) = %q", got)
	}
	if got := timeEmoji(""); got != "?" {
		t.Errorf("timeEmoji(\"\") = %q", got)
	}
}
