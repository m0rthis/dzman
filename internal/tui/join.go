package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/m0rthis/dzman/internal/api"
	"github.com/m0rthis/dzman/internal/config"
	"github.com/m0rthis/dzman/internal/launcher"
	"github.com/m0rthis/dzman/internal/procs"
	"github.com/m0rthis/dzman/internal/steam"
	"github.com/m0rthis/dzman/internal/steamworks"
)

// This file is the terminal counterpart of the TUI's launch flow, behind
// `dzman connect` and `dzman rejoin`. It uses the same pieces as the TUI
// (loadServers, missingMods, resolveBackend, installMissingMods,
// steamworks.Start, waitSteamReleased, prepareLaunch) and only replaces the
// Bubble Tea message plumbing with plain progress lines.

// PasswordEnv names the environment variable a join reads the server
// password from.
const PasswordEnv = "DZMAN_PASSWORD"

// ErrInterrupted is Join's error when SIGINT, SIGTERM or SIGHUP stopped it.
var ErrInterrupted = errors.New("interrupted")

// JoinTarget is the server to join.
type JoinTarget struct {
	IP   string
	Port int
	// ByGamePort says Port is the game port (`dzman connect`, like the TUI's
	// Direct Connect). Otherwise it is the query port, which is what the
	// history and favourites store.
	ByGamePort bool
	// Name is shown while the server is not found in the list.
	Name string
}

// ErrNoHistory is RejoinTarget's error for a profile with no history.
var ErrNoHistory = errors.New("no server to rejoin: the history is empty (join a server with dzman first)")

// RejoinTarget is the most recently played server in the profile's history.
func RejoinTarget(p *config.Profile) (JoinTarget, error) {
	if len(p.History) == 0 {
		return JoinTarget{}, ErrNoHistory
	}
	h := p.History[0]
	return JoinTarget{IP: h.IP, Port: h.Port, Name: h.Name}, nil
}

// findServer returns the server t names in list, or nil.
func findServer(list []api.Server, t JoinTarget) *api.Server {
	for i := range list {
		s := &list[i]
		if s.Endpoint.IP != t.IP {
			continue
		}
		if (t.ByGamePort && s.GamePort == t.Port) || (!t.ByGamePort && s.Endpoint.Port == t.Port) {
			return s
		}
	}
	return nil
}

// JoinOptions is everything Join needs.
type JoinOptions struct {
	Cfg     *config.Config
	Profile *config.Profile
	// Paths is nil when the Steam/DayZ folders were not found.
	Paths  *steam.Paths
	Target JoinTarget
	// Password is the server password, if it needs one.
	Password string
	Out      io.Writer
}

// Join syncs the target server's mods through the configured backend, makes
// sure the Steamworks helper is gone, records the join and launches DayZ.
// Progress goes to opts.Out. On any error nothing is launched. A running
// helper and steamcmd are stopped on every way out. A first SIGINT, SIGTERM
// or SIGHUP asks them to stop and waits for it; a second one kills steamcmd
// at once and exits.
func Join(opts JoinOptions) error {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	handlerDone := make(chan struct{})
	var stopOnce sync.Once
	// stop ends the signal handling. It runs before the launch, once there is
	// nothing left to clean up, and when Join returns. A signal that arrived
	// but was not handled yet counts as an interrupt.
	stop := func() {
		stopOnce.Do(func() {
			signal.Stop(sigs)
			close(stopped)
		})
		<-handlerDone // a signal it already took has been acted on
		select {
		case <-sigs:
			cancel()
		default:
		}
	}
	defer stop()
	defer steamworks.CancelAll()
	go func() {
		defer close(handlerDone)
		select {
		case <-sigs:
		case <-stopped:
			return
		}
		cancel()
		fmt.Fprintln(os.Stderr, "\ndzman: stopping…, press Ctrl+C again to force")
		go steamworks.CancelAll()
		select {
		case <-sigs:
			// The helper gets SIGTERM from Pdeathsig as we exit.
			steam.KillStopping()
			os.Exit(130)
		case <-stopped:
		}
	}()
	err := join(ctx, stop, opts)
	if err != nil && ctx.Err() != nil {
		return ErrInterrupted // whatever the interrupt turned into on the way out
	}
	return err
}

// serverListResult is a server list and where it came from.
type serverListResult struct {
	list *api.ServerList
	// fetched: downloaded just now, not read from the cache.
	fetched bool
	// stale: the download failed and this is the cached list, however old.
	stale bool
}

// loadServersForJoin is loadServers for the terminal join: with the network
// down it falls back to the cached list whatever its age.
func loadServersForJoin(ctx context.Context, cfg *config.Config, force bool) (serverListResult, error) {
	if !force {
		if sl, ok := api.LoadCache[api.ServerList](cfg.ServersCachePath, cfg.ServerDBTTL.Std()); ok {
			return serverListResult{list: sl}, nil
		}
	}
	sl, err := fetchServers(ctx, cfg)
	if err == nil {
		return serverListResult{list: sl, fetched: true}, nil
	}
	if ctx.Err() == nil {
		if old, ok := api.LoadCache[api.ServerList](cfg.ServersCachePath, time.Duration(1<<62)); ok {
			return serverListResult{list: old, stale: true}, nil
		}
	}
	return serverListResult{}, err
}

// serverLoader loads the server list; a variable so tests can replace it.
var serverLoader = loadServersForJoin

// startDayZ starts the game and calls started once it is on its way; a
// variable so tests can replace it.
var startDayZ = launcher.LaunchDayZNotify

// SanitizeText drops control characters (ESC included) from text that came
// from a server, the API or a helper, so it cannot drive the terminal when
// printed.
func SanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// lookupServer finds the target in the server list. When the cached list does
// not have it, the list is fetched once more before giving up, in case the
// server was listed after the cache was taken. A nil server with a nil error
// means the server is not listed.
func lookupServer(ctx context.Context, out io.Writer, cfg *config.Config, t JoinTarget) (*api.Server, error) {
	res, err := serverLoader(ctx, cfg, false)
	if err != nil {
		return nil, fmt.Errorf("load server list: %w", err)
	}
	if res.stale {
		fmt.Fprintln(out, "Could not refresh the server list; using the cached one, which may be out of date")
	}
	if srv := findServer(res.list.Result, t); srv != nil {
		return srv, nil
	}
	if res.fetched || res.stale {
		return nil, nil // nothing newer to look at
	}
	fresh, err := serverLoader(ctx, cfg, true)
	if err != nil {
		return nil, fmt.Errorf("the cached server list does not have %s:%d and refreshing it failed: %w",
			t.IP, t.Port, err)
	}
	if fresh.stale {
		return nil, nil
	}
	return findServer(fresh.list.Result, t), nil
}

var errNotSetUp = errors.New("dzman is not set up (DayZ folder not found); run `dzman` once to finish setup")

func join(ctx context.Context, stopSignals func(), o JoinOptions) error {
	out := o.Out
	srv, err := lookupServer(ctx, out, o.Cfg, o.Target)
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	if err != nil && !o.Target.ByGamePort {
		return fmt.Errorf("%w (rejoin needs the server list to find the game port)", err)
	}
	if srv == nil {
		if !o.Target.ByGamePort {
			return fmt.Errorf("%s:%d is not in the server list, so its mods and game port are unknown",
				o.Target.IP, o.Target.Port)
		}
		// Like the TUI's Direct Connect: an unlisted server is launched with
		// no mods and is not added to the history.
		if err != nil {
			fmt.Fprintf(out, "%s; launching without mods\n", SanitizeText(err.Error()))
		} else {
			fmt.Fprintf(out, "%s:%d is not in the server list; launching without mods\n", SanitizeText(o.Target.IP), o.Target.Port)
		}
		args := launcher.ConnectArgs{
			ServerIP: o.Target.IP, GamePort: o.Target.Port, Password: o.Password,
			PlayerName: o.Profile.Player, ExtraArgs: o.Profile.LaunchArgs(),
		}
		return launch(ctx, stopSignals, out, o.Profile.Player, args.ToSteamArgs(), nil)
	}
	fmt.Fprintf(out, "Server: %s (%s:%d)\n", SanitizeText(srv.Name), SanitizeText(srv.Endpoint.IP), srv.GamePort)
	if srv.Password && o.Password == "" {
		return fmt.Errorf("the server needs a password; set %s", PasswordEnv)
	}
	if len(srv.Mods) > 0 && o.Paths == nil {
		return errNotSetUp // mods can be neither synced nor linked
	}
	if missing := missingMods(o.Paths, srv); len(missing) > 0 {
		if err := syncMods(ctx, o, srv, missing); err != nil {
			if ctx.Err() != nil {
				return ErrInterrupted
			}
			return err
		}
	} else if len(srv.Mods) > 0 {
		fmt.Fprintf(out, "All %d mods are installed\n", len(srv.Mods))
	}
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	args, err := prepareLaunch(o.Cfg, o.Profile, o.Paths, srv, o.Password)
	if err != nil {
		return err
	}
	// Recorded as soon as the steam command has started, so a signal during
	// the wait for the game to appear keeps it.
	return launch(ctx, stopSignals, out, o.Profile.Player, args, func() { recordJoin(o.Cfg, o.Profile, srv) })
}

// waitReleased waits for the Steam client to let go of DayZ (see
// waitSteamReleased), cutting the wait short when ctx is cancelled.
func waitReleased(ctx context.Context) {
	waitSteamReleased(
		func(dur time.Duration) {
			select {
			case <-time.After(dur):
			case <-ctx.Done():
			}
		},
		time.Now,
		func() bool { return ctx.Err() == nil && gameOrHelperRunning() },
	)
}

// launch starts DayZ once no Steamworks helper is left.
func launch(ctx context.Context, stopSignals func(), out io.Writer, player string, args []string, started func()) error {
	// A helper still running here would make the Steam client think DayZ is
	// running already. Every path to this point has waited for it; this
	// makes sure.
	steamworks.CancelAll()
	if launcher.IsDayZRunning() {
		return errors.New("DayZ is already running")
	}
	// The client may still count DayZ as running if another dzman's helper
	// just exited.
	if registrySaysRunning(steam.RegistryFiles()) {
		fmt.Fprintln(out, "Waiting for the Steam client to let go of DayZ…")
		waitReleased(ctx)
	}
	// Nothing is left to clean up: stop catching signals, then see whether
	// one came in on the way. If so, do not launch.
	stopSignals()
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	fmt.Fprintln(out, "Launching DayZ…")
	return startDayZ(player, args, started)
}

// syncMods installs the missing mods with the configured backend.
func syncMods(ctx context.Context, o JoinOptions, srv *api.Server, missing []api.Mod) error {
	out := o.Out
	fmt.Fprintf(out, "%d of %d mods are missing:\n", len(missing), len(srv.Mods))
	for _, m := range missing {
		fmt.Fprintf(out, "  %s\n", SanitizeText(m.Name))
	}
	path, libErr := steamworks.FindLibrary(libFindOptions(o.Cfg, o.Paths))
	in := gatherBackendInputs(o.Cfg, o.Profile, o.Paths, libLookup{path: path, err: libErr})
	backend, why := resolveBackend(in)
	fmt.Fprintf(out, "Mod backend: %s → %s\n", in.mode, backend)
	if why != "" {
		fmt.Fprintln(out, SanitizeText(why))
	}
	switch backend {
	case backendSteamworks:
		return syncSteamworks(ctx, o, in, missing)
	case backendSteamCMD:
		return syncSteamCMD(ctx, o, in, missing)
	}
	fmt.Fprintln(out, "Subscribe to these in Steam, then run dzman again:")
	for _, m := range missing {
		fmt.Fprintf(out, "  %s\n", SanitizeText(m.WorkshopURL()))
	}
	return fmt.Errorf("%d mods are missing and the mod backend is manual", len(missing))
}

func syncSteamCMD(ctx context.Context, o JoinOptions, in backendInputs, missing []api.Mod) error {
	if !in.steamCMDReady() {
		return errors.New("SteamCMD: " + in.steamCMDProblem())
	}
	fmt.Fprintf(o.Out, "Installing %d mods with SteamCMD…\n", len(missing))
	bad, err := installMissingMods(ctx, in.cmdPath, o.Profile.SteamLogin, o.Paths, missing)
	for _, b := range bad {
		fmt.Fprintln(o.Out, SanitizeText(b))
	}
	if errors.Is(err, steam.ErrLoginRequired) {
		return errors.New("SteamCMD needs a login; run `dzman` and use Config > Change Steam Login, then press l on the missing-mods page")
	}
	if err != nil {
		return err
	}
	return stillMissing(o, missing)
}

// stillMissing errors if any of the mods is still not installed.
func stillMissing(o JoinOptions, mods []api.Mod) error {
	left := missingMods(o.Paths, &api.Server{Mods: mods})
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("%d mods still missing after the install", len(left))
}

func syncSteamworks(ctx context.Context, o JoinOptions, in backendInputs, missing []api.Mod) error {
	out := o.Out
	fail := func(err error) error { return fmt.Errorf("Steamworks: %w", err) }
	if !launcher.IsSteamRunning() {
		return fail(errors.New("the Steam client is not running; start Steam, log in, and try again"))
	}
	if launcher.IsDayZRunning() {
		return fail(errors.New("DayZ is running; close it first"))
	}
	if steamworks.IsHelperRunning(procs.ByArgv) {
		return fail(errors.New("another Steamworks download is running (another dzman?); wait for it or cancel it"))
	}
	if in.libErr != nil {
		return fail(in.libErr)
	}
	ids, names := modNames(missing)
	d := newDLState(dlInstall, ids, names)
	valid := d.validIDs()
	if len(valid) == 0 {
		return fail(errors.New("no valid workshop IDs: " + strings.Join(d.failures(), "; ")))
	}
	exe, err := steamworks.HelperExe()
	if err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	job, err := steamworks.Start(steamworks.JobOptions{
		Exe:       exe,
		Lib:       in.lib,
		IDs:       valid,
		Subscribe: true,
		Timeout:   o.Cfg.SteamworksTimeout.Std(),
		Stall:     o.Cfg.SteamworksStall.Std(),
	})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "Downloading %d mods with the Steam client (DayZ shows as running in Steam meanwhile)…\n", len(valid))
	// A signal that came in before the job was registered found nothing for
	// CancelAll to stop, so cancel here as well.
	if ctx.Err() != nil {
		job.Cancel()
	}
	pct := map[string]uint64{}
	handle := func(ev steamworks.Event) {
		d.apply(ev)
		it := d.items[ev.ID]
		if it == nil {
			return
		}
		switch ev.Type {
		case steamworks.EventProgress:
			if ev.Total > 0 {
				p := min(ev.Downloaded*100/ev.Total, 100) / 10 * 10
				if last, seen := pct[ev.ID]; !seen || p > last {
					pct[ev.ID] = p
					fmt.Fprintf(out, "  %s: %d%% (%s / %s)\n", SanitizeText(it.name), min(ev.Downloaded*100/ev.Total, 100),
						formatBytes(int64(ev.Downloaded)), formatBytes(int64(ev.Total)))
				}
			}
		case steamworks.EventDone:
			fmt.Fprintf(out, "  %s: installed\n", SanitizeText(it.name))
			if ev.Folder != "" && !searchesFolder(o.Paths, ev.Folder) {
				it.elsewhere = ev.Folder
			}
		case steamworks.EventError:
			fmt.Fprintf(out, "  %s: failed: %s\n", SanitizeText(it.name), SanitizeText(ev.Error))
		case steamworks.EventSkipped:
			fmt.Fprintf(out, "  %s: skipped (%s)\n", SanitizeText(it.name), SanitizeText(ev.Error))
		}
	}
	cancelled := ctx.Done()
	for events := job.Events; events != nil; {
		select {
		case <-cancelled:
			job.Cancel()
			cancelled = nil // keep draining until the helper is gone
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			handle(ev)
		}
	}
	exitErr := job.Wait()
	// The helper is gone; give the Steam client time to let go of DayZ
	// before anything launches it (same wait as the TUI).
	fmt.Fprintln(out, "Closing the Steam connection…")
	waitReleased(ctx)
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	if exitErr != nil {
		return fail(exitErr)
	}
	left := missingMods(o.Paths, &api.Server{Mods: missing})
	if len(left) > 0 {
		msg := fmt.Sprintf("%d mods still missing after the Steam download", len(left))
		if f := d.failures(); len(f) > 0 {
			msg += ": " + strings.Join(f, "; ")
		}
		return fail(errors.New(msg))
	}
	return nil
}
