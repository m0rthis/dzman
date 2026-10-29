package steamworks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// JobOptions configures a helper run.
type JobOptions struct {
	// Exe is the dzman binary to re-exec (see HelperExe).
	Exe string
	// Lib is the libsteam_api.so to load.
	Lib string
	// IDs are the workshop item IDs.
	IDs []string
	// Subscribe subscribes the account to items it is not subscribed to.
	Subscribe bool
	// Timeout is the helper's overall limit; the parent kills it a minute
	// after that. Zero means DefaultTimeout; it is capped at MaxTimeout.
	Timeout time.Duration
	// Stall is the helper's no-progress limit. Zero means DefaultStall.
	Stall time.Duration
}

// Job is a running helper process.
type Job struct {
	// Events delivers the helper's events and is closed when it exits.
	Events <-chan Event

	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	stderr *stderrLog
	// fatal is written by the pump before it returns.
	fatal string
	// cancelled is set by Cancel, so a helper killed on request is not
	// reported as a crash.
	cancelled atomic.Bool
	// abandoned is closed by abandon once nobody reads Events any more, so
	// the pump drops events instead of blocking on a full channel.
	abandoned   chan struct{}
	abandonOnce sync.Once
}

// procSelfExe re-execs the running binary even if its file was replaced
// (an upgrade while dzman runs), where os.Executable's path would name the
// new one.
const procSelfExe = "/proc/self/exe"

// HelperExe is the path to re-exec as the helper: /proc/self/exe when /proc
// has it, else os.Executable().
func HelperExe() (string, error) {
	if _, err := os.Stat(procSelfExe); err == nil {
		return procSelfExe, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find dzman binary: %w", err)
	}
	return exe, nil
}

// exitGrace is how long a job waits, once the helper has exited, for its
// events and stderr pipes to reach EOF. A process the helper (or the
// library) forked may hold them open, and must not keep the job running.
var exitGrace = 2 * time.Second

// ErrCancelled is Wait's error for a helper that Cancel stopped before it
// could exit on its own terms.
var ErrCancelled = errors.New("steamworks download cancelled")

// active tracks running jobs so CancelAll can stop them on exit.
var (
	activeMu sync.Mutex
	active   = map[*Job]bool{}
)

// Active reports whether any helper started by this process is running.
func Active() bool {
	activeMu.Lock()
	defer activeMu.Unlock()
	return len(active) > 0
}

// CancelAll stops every running helper and waits for it to exit. It assumes
// nobody reads the jobs' Events any more (the TUI has quit), so events still
// in flight are dropped rather than blocking the exit.
func CancelAll() {
	activeMu.Lock()
	jobs := make([]*Job, 0, len(active))
	for j := range active {
		jobs = append(jobs, j)
	}
	activeMu.Unlock()
	for _, j := range jobs {
		j.abandon()
		j.Cancel()
		<-j.done
	}
}

// helperArgs builds the helper's command line.
func helperArgs(opts JobOptions) []string {
	args := []string{HelperCommand, "--lib", opts.Lib}
	if opts.Subscribe {
		args = append(args, "--subscribe")
	}
	if opts.Timeout > 0 {
		args = append(args, "--timeout", min(opts.Timeout, MaxTimeout).String())
	}
	if opts.Stall > 0 {
		args = append(args, "--stall", opts.Stall.String())
	}
	args = append(args, "--")
	return append(args, opts.IDs...)
}

// helperEnviron is environ with the app ID variables set for DayZ.
func helperEnviron(environ []string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "SteamAppId=") && !strings.HasPrefix(kv, "SteamGameId=") {
			out = append(out, kv)
		}
	}
	return append(out, "SteamAppId="+AppID, "SteamGameId="+AppID)
}

// Start launches the helper. Read Events until it is closed, then call Wait.
func Start(opts JobOptions) (*Job, error) {
	if len(opts.IDs) == 0 {
		return nil, errors.New("no workshop items to download")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	timeout = min(timeout, MaxTimeout) // so timeout+time.Minute cannot overflow
	// Real pipes rather than StdoutPipe and an io.Writer for stderr: with
	// *os.File ends exec starts no copy goroutines, so Wait returns as soon
	// as the helper exits even if a process it forked still holds a pipe.
	// The job reads both itself and closes them in finish.
	evR, evW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("steamworks helper: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(evR, evW)
		return nil, fmt.Errorf("steamworks helper: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
	cmd := exec.CommandContext(ctx, opts.Exe, helperArgs(opts)...)
	if opts.Exe == procSelfExe {
		cmd.Args[0] = "dzman" // so isHelperArgv still recognises the helper
	}
	cmd.Env = helperEnviron(os.Environ())
	// SIGTERM first so the helper can call SteamAPI_Shutdown; SIGKILL if it
	// is still there 15s later.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	// If dzman dies the helper gets SIGTERM instead of lingering as "DayZ".
	// Pdeathsig fires when the thread that forked exits, not the process, so
	// the child is started from a goroutine locked to its thread that stays
	// until the child is reaped.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cmd.Stdout, cmd.Stderr = evW, errW

	events := make(chan Event, 64)
	j := &Job{
		Events: events, cmd: cmd, cancel: cancel, done: make(chan struct{}),
		stderr: &stderrLog{max: 4096}, abandoned: make(chan struct{}),
	}
	started := make(chan error, 1)
	go func() {
		// Never unlocked: the thread exits with this goroutine, after Wait.
		runtime.LockOSThread()
		err := cmd.Start()
		closeAll(evW, errW) // the helper has its own copies
		if err != nil {
			cancel()
			closeAll(evR, errR)
			started <- fmt.Errorf("start steamworks helper: %w", err)
			return
		}
		activeMu.Lock()
		active[j] = true
		activeMu.Unlock()
		started <- nil

		pumpDone := make(chan struct{})
		var decodeErr error
		go func() {
			decodeErr = j.pump(evR, events)
			close(pumpDone)
		}()
		stderrDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(j.stderr, errR)
			close(stderrDone)
		}()
		waitErr := cmd.Wait()
		if j.drain(evR, errR, pumpDone, stderrDone) {
			decodeErr = nil // cut off by us, not a broken stream
		}
		j.finish(ctx, waitErr, decodeErr)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return j, nil
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

// pump forwards the helper's events until the summary (the last event),
// EOF, or finish closing the pipe. It closes events.
func (j *Job) pump(r io.Reader, events chan<- Event) error {
	defer close(events)
	err := decode(r, func(ev Event) bool {
		switch ev.Type {
		case EventFatal:
			j.fatal = ev.Error
		}
		select {
		case events <- ev:
		case <-j.abandoned:
		}
		return ev.Type != EventSummary
	})
	if err != nil {
		// Keep reading so a chatty helper cannot block on a full pipe. This
		// ends at EOF or when finish closes the pipe.
		_, _ = io.Copy(io.Discard, r)
	}
	return err
}

// drain runs once the helper has exited. It gives the pipes exitGrace to
// reach EOF, then closes them so a lingering child cannot hold the job
// open. cutOff reports whether the events pipe was closed under the pump.
func (j *Job) drain(evR, errR *os.File, pumpDone, stderrDone <-chan struct{}) (cutOff bool) {
	graceOver := make(chan struct{})
	timer := time.AfterFunc(exitGrace, func() { close(graceOver) })
	defer timer.Stop()
	select {
	case <-pumpDone:
	case <-graceOver:
		cutOff = true
	}
	_ = evR.Close()
	<-pumpDone
	select {
	case <-stderrDone:
	case <-graceOver:
	}
	_ = errR.Close()
	<-stderrDone
	return cutOff
}

// finish records the helper's outcome and marks the job done.
func (j *Job) finish(ctx context.Context, waitErr, decodeErr error) {
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	j.cancel()
	activeMu.Lock()
	delete(active, j)
	activeMu.Unlock()
	j.err = j.exitError(timedOut, waitErr, decodeErr)
	close(j.done)
}

// abandon tells the pump that nobody reads Events any more.
func (j *Job) abandon() { j.abandonOnce.Do(func() { close(j.abandoned) }) }

// exitError turns the helper's exit into an error, preferring its own
// fatal message.
func (j *Job) exitError(timedOut bool, waitErr, decodeErr error) error {
	switch {
	case j.fatal != "":
		return errors.New(j.fatal)
	case timedOut:
		if killedBySignal(waitErr) {
			return errors.New("steamworks helper timed out and was killed")
		}
		// It exited on its own after the SIGTERM at the deadline.
		return errors.New("steamworks helper timed out")
	case waitErr != nil:
		if j.cancelled.Load() {
			return ErrCancelled
		}
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 {
			return nil // some items failed; their error events say why
		}
		if err := j.crashError(ee); err != nil {
			return err
		}
		if t := j.stderr.lastLine(); t != "" {
			return fmt.Errorf("steamworks helper: %w: %s", waitErr, t)
		}
		return fmt.Errorf("steamworks helper: %w", waitErr)
	}
	return decodeErr
}

// killedBySignal reports whether waitErr is the exit of a process a signal
// killed (rather than one that handled the signal and exited).
func killedBySignal(waitErr error) bool {
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// crashError describes a helper killed by a signal or by a Go runtime crash
// (a panic, or a fault inside the library). It is nil for other exits.
func (j *Job) crashError(ee *exec.ExitError) error {
	if ee == nil {
		return nil
	}
	if c := j.stderr.crashLine(); c != "" {
		return fmt.Errorf("steamworks helper crashed (%s)", c)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return nil
	}
	if t := j.stderr.lastLine(); t != "" {
		return fmt.Errorf("steamworks helper crashed (%s); its last output: %s", signalName(ws.Signal()), t)
	}
	return fmt.Errorf("steamworks helper crashed (%s)", signalName(ws.Signal()))
}

// signalNames are the signals a crash or a kill usually shows.
var signalNames = map[syscall.Signal]string{
	syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS",
	syscall.SIGILL: "SIGILL", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
	syscall.SIGTERM: "SIGTERM", syscall.SIGTRAP: "SIGTRAP",
}

func signalName(sig syscall.Signal) string {
	if n, ok := signalNames[sig]; ok {
		return n
	}
	return sig.String()
}

// Cancel asks the helper to stop (SIGTERM, then SIGKILL after 15s).
func (j *Job) Cancel() {
	j.cancelled.Store(true)
	j.cancel()
}

// Wait blocks until the helper has exited and returns why it failed as a
// whole. Per-item failures arrive as EventError, not here.
func (j *Job) Wait() error {
	<-j.done
	return j.err
}

// stderrLog keeps the last max bytes of the helper's stderr and the first
// line that looks like a Go runtime crash report.
type stderrLog struct {
	mu      sync.Mutex
	max     int
	buf     []byte
	partial []byte // the current unterminated line, capped
	crash   string
}

// crashPrefixes start the line the Go runtime prints when the process dies.
var crashPrefixes = []string{"panic: ", "fatal error: ", "SIGSEGV: ", "SIGBUS: ", "SIGILL: ", "SIGFPE: ", "SIGABRT: "}

// maxCrashLine caps the crash line kept from stderr.
const maxCrashLine = 512

func (t *stderrLog) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	for rest := p; t.crash == "" && len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			t.partial = appendCapped(t.partial, rest)
			break
		}
		line := strings.TrimSpace(string(appendCapped(t.partial, rest[:i])))
		t.partial, rest = t.partial[:0], rest[i+1:]
		for _, pre := range crashPrefixes {
			if strings.HasPrefix(line, pre) {
				t.crash = line
				break
			}
		}
	}
	return len(p), nil
}

func appendCapped(dst, src []byte) []byte {
	if room := maxCrashLine - len(dst); len(src) > room {
		src = src[:max(room, 0)]
	}
	return append(dst, src...)
}

func (t *stderrLog) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(t.buf), []byte("\n"))
	return strings.TrimSpace(string(lines[len(lines)-1]))
}

func (t *stderrLog) crashLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.crash
}

// IsHelperRunning reports whether any helper process of this user (from any
// dzman) is running. byArgv returns the PIDs whose argv matches (see
// procs.ByArgv). Another user's helper does not hold our Steam session.
func IsHelperRunning(byArgv func(func(argv []string) bool) []int) bool {
	if Active() {
		return true
	}
	uid := uint32(os.Getuid())
	for _, pid := range byArgv(isHelperArgv) {
		if procOwnedBy(pid, uid) {
			return true
		}
	}
	return false
}

// procOwnedBy reports whether /proc/<pid> belongs to uid.
func procOwnedBy(pid int, uid uint32) bool {
	fi, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}

// isHelperArgv matches a helper's command line: argv[0] is a dzman binary
// (named dzman, or named like this one) and argv[1] is HelperCommand.
func isHelperArgv(argv []string) bool {
	if len(argv) < 2 || argv[1] != HelperCommand {
		return false
	}
	name := filepath.Base(argv[0])
	if name == "dzman" {
		return true
	}
	self, err := os.Executable()
	return err == nil && name == filepath.Base(self)
}
