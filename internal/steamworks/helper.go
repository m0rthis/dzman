// Package steamworks downloads workshop mods through the running Steam
// client using the Steamworks flat API in libsteam_api.so, loaded at runtime
// with purego (no cgo). The API is only ever initialised in a short-lived
// helper process (see HelperCommand), never in the TUI.
package steamworks

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// HelperCommand is the hidden dzman subcommand that runs the helper:
//
//	dzman steamworks-download --lib <libsteam_api.so> [--subscribe] [--timeout 30m] [--stall 5m] [--subscribe-wait 45s] <id>...
//
// It initialises the Steam API as DayZ (221100), asks the running Steam
// client to download each workshop item, reports progress as JSON lines
// (see Event) and exits. It runs in its own process because while the API
// is initialised the Steam client counts DayZ as running.
const HelperCommand = "steamworks-download"

// AppID is the Steam app the helper identifies as.
const AppID = "221100"

// Defaults for the helper's limits.
const (
	DefaultTimeout = 30 * time.Minute
	DefaultStall   = 5 * time.Minute
	// DefaultSubscribeWait is how long an item the helper subscribed to may
	// show neither the subscribed flag nor any download activity before it
	// fails. A removed, private or hidden item stays at state 0 and would
	// otherwise only fail at the stall, with DayZ shown as running all along.
	DefaultSubscribeWait = 45 * time.Second
	// MaxTimeout caps the overall limit, so deadlines computed from it
	// cannot overflow.
	MaxTimeout = 24 * time.Hour
)

// RunHelper is the entry point for `dzman steamworks-download`. args are the
// arguments after the subcommand. It returns the process exit code: 0 when
// every item installed, 1 when some failed, 2 when the helper could not run.
func RunHelper(args []string) int {
	// The Steam API expects init, callbacks and shutdown on one thread.
	runtime.LockOSThread()

	// A broken stderr or events pipe (the parent died) must not kill the
	// helper before SteamAPI_Shutdown; writes fail with EPIPE instead.
	signal.Ignore(syscall.SIGPIPE)

	// Catch signals before anything slow (load, init) so a cancel at any
	// point still ends in SteamAPI_Shutdown and "cancelled" events.
	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-sigs
		close(stop)
	}()

	events := redirectStdout()
	enc := NewEncoder(events)
	fatal := func(err error) int {
		_ = enc.Encode(Event{Type: EventFatal, Error: err.Error()})
		return 2
	}

	cfg, lib, err := parseHelperArgs(args)
	if err != nil {
		return fatal(err)
	}
	cfg.stop = stop

	// The parent sets these too; setenv here covers a manual run.
	_ = os.Setenv("SteamAppId", AppID)
	_ = os.Setenv("SteamGameId", AppID)

	api, err := loadAPI(lib)
	if err != nil {
		return fatal(err)
	}
	if err := api.init(); err != nil {
		return fatal(err)
	}

	res := run(apiClient{api}, cfg, enc)
	api.shutdown()
	_ = enc.Encode(Event{Type: EventSummary, OK: res.ok, Failed: res.failed, Skipped: res.skipped})
	if res.failed > 0 {
		return 1
	}
	return 0
}

// parseHelperArgs parses the helper's command line into a runConfig and the
// library path.
func parseHelperArgs(args []string) (runConfig, string, error) {
	fs := flag.NewFlagSet(HelperCommand, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	lib := fs.String("lib", "", "absolute path to libsteam_api.so")
	subscribe := fs.Bool("subscribe", false, "subscribe to items that are not subscribed (else skip them)")
	timeout := fs.Duration("timeout", DefaultTimeout, "overall time limit")
	stall := fs.Duration("stall", DefaultStall, "give up after this long without progress")
	subscribeWait := fs.Duration("subscribe-wait", DefaultSubscribeWait,
		"fail an item we subscribed to after this long with no subscription and no download activity")
	if err := fs.Parse(args); err != nil {
		return runConfig{}, "", err
	}
	if *lib == "" {
		return runConfig{}, "", fmt.Errorf("--lib is required")
	}
	if !filepath.IsAbs(*lib) {
		// A relative or bare name would make dlopen search the library path
		// instead of loading the file that was inspected.
		return runConfig{}, "", fmt.Errorf("--lib must be an absolute path, got %q", *lib)
	}
	ids, err := parseIDs(fs.Args())
	if err != nil {
		return runConfig{}, "", err
	}
	return runConfig{
		ids:           ids,
		subscribe:     *subscribe,
		timeout:       *timeout,
		stall:         *stall,
		subscribeWait: *subscribeWait,
	}, *lib, nil
}

// redirectStdout keeps the real stdout for events and points fd 1 at stderr,
// since libsteam_api.so prints to stdout and its chatter must not break a
// JSON line. The events fd is close-on-exec so processes the library starts
// (steamclient helpers) do not inherit it and hold the pipe open. If the
// redirect fails, events go to stdout as before.
func redirectStdout() io.Writer {
	fd, _, errno := syscall.Syscall(syscall.SYS_FCNTL, 1, syscall.F_DUPFD_CLOEXEC, 3)
	if errno != 0 {
		return os.Stdout
	}
	if err := syscall.Dup3(2, 1, 0); err != nil {
		_ = syscall.Close(int(fd))
		return os.Stdout
	}
	return os.NewFile(fd, "events")
}

func parseIDs(args []string) ([]uint64, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("no workshop IDs given")
	}
	ids := make([]uint64, 0, len(args))
	for _, a := range args {
		id, err := strconv.ParseUint(a, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("invalid workshop ID %q", a)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ugcClient is the part of ISteamUGC the download loop uses.
type ugcClient interface {
	subscribe(id uint64) bool
	download(id uint64) bool
	state(id uint64) uint32
	progress(id uint64) (downloaded, total uint64)
	installFolder(id uint64) string
	runCallbacks()
}

// apiClient adapts steamAPI to ugcClient.
type apiClient struct{ a *steamAPI }

// subscribe returns false when SubscribeItem gives k_uAPICallInvalid (0).
func (c apiClient) subscribe(id uint64) bool            { return c.a.subscribeItem(c.a.ugc, id) != 0 }
func (c apiClient) download(id uint64) bool             { return c.a.downloadItem(c.a.ugc, id, true) }
func (c apiClient) state(id uint64) uint32              { return c.a.state(id) }
func (c apiClient) progress(id uint64) (uint64, uint64) { return c.a.progress(id) }
func (c apiClient) installFolder(id uint64) string      { return c.a.installFolder(id) }
func (c apiClient) runCallbacks()                       { c.a.runCallbacks() }

// runConfig controls run. Zero durations get defaults.
type runConfig struct {
	ids []uint64
	// subscribe subscribes to items that are not subscribed. Without it
	// those items are skipped: Steam may uninstall an unsubscribed item once
	// the "game" exits, so downloading it would not stick.
	subscribe bool
	timeout   time.Duration
	stall     time.Duration
	// subscribeWait fails an item run subscribed to once it has gone this
	// long without the subscribed flag and without download activity.
	subscribeWait time.Duration
	// settle is how long an item that was already installed and idle must
	// stay idle before it counts as done, since the client may take a
	// moment to flag an update after DownloadItem.
	settle time.Duration
	poll   time.Duration
	// reportEvery throttles progress events per item.
	reportEvery time.Duration
	stop        <-chan struct{}
	now         func() time.Time
	sleep       func(time.Duration)
}

func (c *runConfig) fill() {
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	c.timeout = min(c.timeout, MaxTimeout)
	if c.stall <= 0 {
		c.stall = DefaultStall
	}
	if c.subscribeWait <= 0 {
		c.subscribeWait = DefaultSubscribeWait
	}
	if c.settle <= 0 {
		c.settle = 5 * time.Second
	}
	if c.poll <= 0 {
		c.poll = 250 * time.Millisecond
	}
	if c.reportEvery <= 0 {
		c.reportEvery = time.Second
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
}

type trackedItem struct {
	id         uint64
	name       string
	installed0 bool // installed and idle when we started
	// subscribedByUs is set when run called SubscribeItem for the item, so
	// it only counts as done once the client reports it subscribed.
	subscribedByUs bool
	subscribedAt   time.Time
	sawBusy        bool
	sawBytes       bool // the download counter moved
	lastState      uint32
	lastDL         uint64
	lastReport     time.Time
}

// runResult counts how run ended per item.
type runResult struct {
	ok, failed, skipped int
}

// NotSubscribedMsg is the EventSkipped reason for an unsubscribed item when
// the helper runs without --subscribe.
const NotSubscribedMsg = "not subscribed: install it from a server page or subscribe in Steam"

// SubscriptionFailedMsg is the error for an item the helper subscribed to
// that Steam never reported as subscribed.
const SubscriptionFailedMsg = "subscription did not take: Steam never marked the item subscribed; subscribe in Steam and try again"

// noResponseMsg is the reason for an item that failed the subscribe wait.
func noResponseMsg(wait time.Duration) string {
	return fmt.Sprintf("no subscription or download after %s: the item may be removed, private or hidden", wait)
}

// unfinishedMsg is the error for an item still active when run gives up,
// reason being why it gave up.
func unfinishedMsg(it *trackedItem, reason string) string {
	if it.subscribedByUs && it.lastState&ItemSubscribed == 0 {
		return SubscriptionFailedMsg + " (" + reason + ")"
	}
	return reason
}

// run asks the client for every item and polls until each is installed,
// fails, stalls, times out or stop closes. It emits start, progress, done,
// skipped and error events and returns the counts.
func run(c ugcClient, cfg runConfig, enc *Encoder) (res runResult) {
	cfg.fill()
	start := cfg.now()
	deadline := start.Add(cfg.timeout)
	lastActivity := start

	fail := func(it *trackedItem, msg string) {
		_ = enc.Encode(Event{Type: EventError, ID: it.name, State: it.lastState, Error: msg})
		res.failed++
	}
	stopped := func() bool {
		select {
		case <-cfg.stop:
			return true
		default:
			return false
		}
	}

	var active []*trackedItem
	for _, id := range cfg.ids {
		it := &trackedItem{id: id, name: strconv.FormatUint(id, 10)}
		st := c.state(id)
		it.lastState = st
		it.installed0 = st&ItemInstalled != 0 && st&itemBusy == 0
		_ = enc.Encode(Event{Type: EventStart, ID: it.name, State: st})
		if stopped() {
			fail(it, "cancelled")
			continue
		}
		if st&ItemSubscribed == 0 {
			if !cfg.subscribe {
				_ = enc.Encode(Event{Type: EventSkipped, ID: it.name, State: st, Error: NotSubscribedMsg})
				res.skipped++
				continue
			}
			if !c.subscribe(id) {
				fail(it, "SubscribeItem failed")
				continue
			}
			it.subscribedByUs, it.subscribedAt = true, cfg.now()
		}
		if !c.download(id) {
			fail(it, "DownloadItem refused the item (bad ID, or the client is not logged in)")
			continue
		}
		active = append(active, it)
	}

	for len(active) > 0 {
		if stopped() {
			for _, it := range active {
				fail(it, "cancelled")
			}
			return res
		}
		now := cfg.now()
		if now.After(deadline) {
			for _, it := range active {
				fail(it, unfinishedMsg(it, fmt.Sprintf("timed out after %s", cfg.timeout)))
			}
			return res
		}

		c.runCallbacks()
		remaining := active[:0]
		for _, it := range active {
			st := c.state(it.id)
			dl, total := c.progress(it.id)
			changed := st != it.lastState || dl != it.lastDL
			if changed {
				lastActivity = now
			}
			if st&itemBusy != 0 {
				it.sawBusy = true
			}
			if dl != it.lastDL {
				it.sawBytes = true
			}
			idle := st&ItemInstalled != 0 && st&itemBusy == 0
			if it.subscribedByUs && st&ItemSubscribed == 0 {
				idle = false // installed, but the subscription has not taken yet
			}
			if idle && (!it.installed0 || it.sawBusy || now.Sub(start) >= cfg.settle) {
				_ = enc.Encode(Event{Type: EventDone, ID: it.name, State: st, Folder: c.installFolder(it.id)})
				res.ok++
				continue
			}
			if it.subscribedByUs && st&ItemSubscribed == 0 && !it.sawBusy && !it.sawBytes &&
				now.Sub(it.subscribedAt) >= cfg.subscribeWait {
				it.lastState = st
				fail(it, SubscriptionFailedMsg+" ("+noResponseMsg(cfg.subscribeWait)+")")
				continue
			}
			if st != it.lastState || (changed && now.Sub(it.lastReport) >= cfg.reportEvery) {
				_ = enc.Encode(Event{Type: EventProgress, ID: it.name, State: st, Downloaded: dl, Total: total})
				it.lastReport = now
			}
			it.lastState, it.lastDL = st, dl
			remaining = append(remaining, it)
		}
		active = remaining
		if len(active) == 0 {
			break
		}
		if now.Sub(lastActivity) > cfg.stall {
			for _, it := range active {
				fail(it, unfinishedMsg(it, fmt.Sprintf("stalled: no download progress for %s", cfg.stall)))
			}
			return res
		}
		cfg.sleep(cfg.poll)
	}
	return res
}
