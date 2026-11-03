package steam

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// DefaultSteamCMD is the stored SteamCMDPath value that means "auto-detect".
const DefaultSteamCMD = "steamcmd"

// homeSteamCMDPaths are the usual per-user install spots, relative to $HOME.
// Valve's tarball ships steamcmd.sh, typically unpacked into ~/steamcmd.
var homeSteamCMDPaths = []string{
	"steamcmd/steamcmd.sh",
	"Steam/steamcmd.sh",
	".steam/steamcmd/steamcmd.sh",
}

// systemSteamCMDPaths are distro package locations (Debian/Ubuntu steamcmd).
var systemSteamCMDPaths = []string{
	"/usr/games/steamcmd",
	"/usr/lib/games/steamcmd/steamcmd.sh",
}

// FindSteamCMD locates the steamcmd binary right now. Search order:
//
//  1. configured, unless empty or the default "steamcmd" (a leading ~/ is
//     expanded; a value with a slash is a path, otherwise a PATH lookup).
//     An explicit setting that cannot be found is an error, never a fallback.
//  2. steamcmd on PATH
//  3. steamcmd.sh on PATH
//  4. the per-user install folders in homeSteamCMDPaths
//  5. the distro locations in systemSteamCMDPaths
//
// Auto-detected candidates (2 to 5) that CheckSteamCMDIsolated would refuse
// for clientRoots are skipped, so a later valid one is still used. If every
// candidate was refused the first refusal is returned. An explicit setting is
// returned as is; the caller checks it.
func FindSteamCMD(configured string, clientRoots ...string) (string, error) {
	home, _ := os.UserHomeDir()
	return findSteamCMD(configured, home, exec.LookPath, clientRoots)
}

func findSteamCMD(configured, home string, lookPath func(string) (string, error), clientRoots []string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" && configured != DefaultSteamCMD {
		p := configured
		if home != "" && strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		if strings.Contains(p, "/") {
			if isExecutableFile(p) {
				return p, nil
			}
		} else if found, err := lookPath(p); err == nil {
			return found, nil
		}
		return "", fmt.Errorf("configured steamcmd not found or not executable: %s", configured)
	}

	var refused error
	usable := func(p string) bool {
		if err := CheckSteamCMDIsolated(p, clientRoots...); err != nil {
			if refused == nil {
				refused = err
			}
			return false
		}
		return true
	}
	for _, name := range []string{"steamcmd", "steamcmd.sh"} {
		if found, err := lookPath(name); err == nil && usable(found) {
			return found, nil
		}
	}
	var tried []string
	if home != "" {
		for _, rel := range homeSteamCMDPaths {
			tried = append(tried, filepath.Join(home, rel))
		}
	}
	tried = append(tried, systemSteamCMDPaths...)
	for _, p := range tried {
		if isExecutableFile(p) && usable(p) {
			return p, nil
		}
	}
	if refused != nil {
		return "", refused
	}
	return "", fmt.Errorf("steamcmd not found (looked for steamcmd and steamcmd.sh on PATH, in %s); "+
		"install it or set its path under Config > Change SteamCMD Path", strings.Join(tried, ", "))
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// --- SteamCMD isolation ---
//
// steamcmd is kept away from the Steam client's files. steamcmd.sh links its
// own steamclient.so into ~/.steam/sdk32 and ~/.steam/sdk64 and writes
// ~/.steam/registry.vdf, which are the same files the Steam client and every
// game load. And without +force_install_dir, workshop_download_item writes
// into steamcmd's own steamapps, so a steamcmd that shares a root with the
// client also rewrites the client's workshop ACF. Both can break the client
// (cloud sync in particular). So dzman runs steamcmd with HOME pointed at a
// dzman-owned dir, installs content into another dzman-owned dir, and refuses
// a steamcmd that lives inside the client's root.

// DataDir returns dzman's data directory ($XDG_DATA_HOME/dzman, by default
// ~/.local/share/dzman).
func DataDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "dzman"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "dzman"), nil
}

// flatpakSteamApp is Flatpak Steam's per-app dir, relative to $HOME.
var flatpakSteamApp = filepath.Join(".var", "app", "com.valvesoftware.Steam")

// FlatpakSteamRoot is where Flatpak Steam keeps its root, relative to $HOME.
var FlatpakSteamRoot = filepath.Join(flatpakSteamApp, "data", "Steam")

// SteamCMDContentDir is the +force_install_dir for steamcmd downloads. Mods
// land in WorkshopDir(SteamCMDContentDir(flatpak)).
//
// It is normally $XDG_DATA_HOME/dzman/steamcmd-content. Flatpak Steam cannot
// see that from inside its sandbox, so the @ID links in DayZ's folder would
// break; when the client is Flatpak Steam (flatpak, see Paths.Flatpak), the
// dir goes inside the Flatpak tree
// (~/.var/app/com.valvesoftware.Steam/data/dzman/steamcmd-content), which the
// sandbox sees, but still outside the client's own steamapps.
func SteamCMDContentDir(flatpak bool) (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	return steamCMDContentDir(home, d, flatpak), nil
}

func steamCMDContentDir(home, dataDir string, flatpak bool) string {
	if flatpak && home != "" {
		return filepath.Join(home, flatpakSteamApp, "data", "dzman", "steamcmd-content")
	}
	return filepath.Join(dataDir, "steamcmd-content")
}

// IsFlatpakClient reports whether dir is inside Flatpak Steam's tree,
// ~/.var/app/com.valvesoftware.Steam (symlinks resolved).
func IsFlatpakClient(dir string) bool {
	home, _ := os.UserHomeDir()
	return isFlatpakClient(home, dir)
}

func isFlatpakClient(home, dir string) bool {
	if home == "" || dir == "" {
		return false
	}
	app := filepath.Join(home, flatpakSteamApp)
	return isWithin(dir, app) || isWithin(resolvePath(dir), resolvePath(app))
}

// findClientRoot returns the Steam client's root directory, which is not
// necessarily the root of the library DayZ is in: a custom Steam root may
// point at a secondary library. It tries, symlinks resolved: customRoot if it has a client
// install marker (steam.sh or ubuntu12_32), then ~/.steam/root,
// ~/.steam/steam, ~/.local/share/Steam and Flatpak Steam's root, taking the
// first with steamapps/libraryfolders.vdf or an install marker, then
// customRoot if it has libraryfolders.vdf. "" if none looks like a client.
func findClientRoot(home, customRoot string) string {
	if customRoot != "" && hasClientInstall(customRoot) {
		return resolvePath(customRoot)
	}
	if home != "" {
		for _, c := range []string{
			filepath.Join(home, ".steam", "root"),
			filepath.Join(home, ".steam", "steam"),
			filepath.Join(home, ".local", "share", "Steam"),
			filepath.Join(home, FlatpakSteamRoot),
		} {
			if r := resolvePath(c); hasClientInstall(r) || hasLibraryFolders(r) {
				return r
			}
		}
	}
	if customRoot != "" && hasLibraryFolders(customRoot) {
		return resolvePath(customRoot)
	}
	return ""
}

// hasClientInstall reports whether dir has the client's own files.
func hasClientInstall(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "steam.sh")); err == nil && fi.Mode().IsRegular() {
		return true
	}
	fi, err := os.Stat(filepath.Join(dir, "ubuntu12_32"))
	return err == nil && fi.IsDir()
}

// hasLibraryFolders reports whether dir has the client's library list.
func hasLibraryFolders(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "steamapps", "libraryfolders.vdf"))
	return err == nil && fi.Mode().IsRegular()
}

// resolvePath resolves symlinks in p. When p does not exist, it resolves the
// longest existing parent and keeps the rest as written.
func resolvePath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolvePath(parent), filepath.Base(p))
}

// isWithin reports whether p is dir or inside it (lexically).
func isWithin(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// SteamCMDHomeDir is the HOME steamcmd runs with, so its ~/.steam writes stay
// out of the real ~/.steam.
func SteamCMDHomeDir() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "steamcmd-home"), nil
}

// WorkshopDir returns the DayZ workshop content dir under a Steam (or
// steamcmd install) root.
func WorkshopDir(root string) string {
	return filepath.Join(root, "steamapps", "workshop", "content", GameID)
}

// ClientRoots returns every directory that may be a Steam client's root:
// the targets of ~/.steam/root and ~/.steam/steam, ~/.local/share/Steam,
// Flatpak Steam's root, and extra (e.g. the detected library's root).
// Empty and duplicate entries are dropped; missing dirs are kept, since
// CheckSteamCMDIsolated compares paths, not contents.
func ClientRoots(extra ...string) []string {
	home, _ := os.UserHomeDir()
	return clientRoots(home, extra...)
}

func clientRoots(home string, extra ...string) []string {
	var cands []string
	if home != "" {
		cands = append(cands,
			filepath.Join(home, ".steam", "root"),
			filepath.Join(home, ".steam", "steam"),
			filepath.Join(home, ".local", "share", "Steam"),
			filepath.Join(home, FlatpakSteamRoot),
		)
	}
	cands = append(cands, extra...)
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		if c == "" {
			continue
		}
		r := resolvePath(c)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// CheckSteamCMDIsolated returns an error if steamcmdPath lives inside any of
// the Steam client roots (see ClientRoots), which means it would share the
// client's files. Symlinks are resolved on both sides.
func CheckSteamCMDIsolated(steamcmdPath string, clientRoots ...string) error {
	bin := resolvePath(steamcmdPath)
	for _, clientRoot := range clientRoots {
		if clientRoot == "" {
			continue
		}
		if root := resolvePath(clientRoot); isWithin(bin, root) {
			return fmt.Errorf("steamcmd at %s is inside the Steam client's folder %s and would share its files; "+
				"install steamcmd somewhere else (e.g. ~/steamcmd) and set it under Config > Change SteamCMD Path", bin, root)
		}
	}
	return nil
}

// envDir is an environment variable naming a dir under the isolated home.
type envDir struct{ name, rel string }

// isolatedVars are the variables isolatedEnv points into the isolated home,
// relative to it. steamcmd and the libraries it loads may follow the XDG
// dirs rather than HOME.
var isolatedVars = []envDir{
	{"HOME", ""},
	{"XDG_DATA_HOME", filepath.Join(".local", "share")},
	{"XDG_CONFIG_HOME", ".config"},
	{"XDG_CACHE_HOME", ".cache"},
	{"XDG_STATE_HOME", filepath.Join(".local", "state")},
}

// isolatedEnv returns environ with HOME and the XDG base dirs replaced by
// dirs under home.
func isolatedEnv(environ []string, home string) []string {
	out := make([]string, 0, len(environ)+len(isolatedVars))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(isolatedVars, func(v envDir) bool { return v.name == name }) {
			out = append(out, kv)
		}
	}
	for _, v := range isolatedVars {
		out = append(out, v.name+"="+filepath.Join(home, v.rel))
	}
	return out
}

// steamCMDCommand builds a steamcmd command that runs with the isolated HOME,
// creating it if needed.
func steamCMDCommand(steamcmdPath string, args ...string) (*exec.Cmd, error) {
	cmd, _, err := steamCMDCommandContext(context.Background(), steamcmdPath, args...)
	return cmd, err
}

// steamCMDCommandContext is steamCMDCommand for a command that ctx can stop.
// The second result must be called once the command has finished (it is a
// no-op for a ctx that can never be cancelled, which gets exactly the plain
// command).
//
// steamcmd.sh and distro wrappers run the real binary as a child, so a
// cancellable command gets its own process group and a cancel signals the
// whole group: SIGTERM, then SIGKILL after steamCMDStopGrace (and at once
// when the command finishes after a cancel, so a child that ignores SIGTERM
// cannot outlive it). Pdeathsig also sends SIGTERM to the command itself if
// dzman dies, but only to the command, not to a child it started.
func steamCMDCommandContext(ctx context.Context, steamcmdPath string, args ...string) (*exec.Cmd, func(), error) {
	home, err := SteamCMDHomeDir()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create steamcmd home: %w", err)
	}
	cmd := exec.CommandContext(ctx, steamcmdPath, args...)
	cmd.Env = isolatedEnv(os.Environ(), home)
	if ctx.Done() == nil {
		return cmd, func() {}, nil
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.WaitDelay = steamCMDWaitDelay
	var (
		mu    sync.Mutex
		pid   int // the group, once a cancel has signalled it
		timer *time.Timer
	)
	cmd.Cancel = func() error {
		mu.Lock()
		pid = cmd.Process.Pid
		timer = time.AfterFunc(steamCMDStopGrace, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		stopping.Store(pid, true)
		mu.Unlock()
		return syscall.Kill(-pid, syscall.SIGTERM)
	}
	return cmd, func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
			// Whatever is left of the group (a child that ignored SIGTERM)
			// goes now.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			stopping.Delete(pid)
		}
	}, nil
}

// stopping holds the process groups of steamcmd commands that are being
// stopped (pid -> true), for KillStopping.
var stopping sync.Map

// KillStopping sends SIGKILL to every steamcmd command that is being stopped
// after a cancel. It is the second Ctrl+C.
func KillStopping() {
	stopping.Range(func(k, _ any) bool {
		_ = syscall.Kill(-k.(int), syscall.SIGKILL)
		return true
	})
}

// --- SteamCMD operations ---

// ErrLoginRequired is returned (wrapped) when steamcmd fails because it has no
// valid cached credentials for the login. Fix it with LoginCmd.
var ErrLoginRequired = errors.New("steamcmd login required")

// loginFailureMarkers are lowercase fragments of steamcmd output that mean the
// login itself failed or needs interactive input (password, Steam Guard).
var loginFailureMarkers = []string{
	"cached credentials not found",
	"invalid password",
	"failed login",
	"login failure",
	"steam guard",
	"two-factor",
	"account logon denied",
	"password:",
}

// IsLoginFailure reports whether steamcmd output indicates a login or
// credential problem. The match is case-insensitive.
func IsLoginFailure(out []byte) bool {
	s := strings.ToLower(string(out))
	for _, marker := range loginFailureMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// LoginCmd builds an interactive `steamcmd +login <name> +quit` with the same
// isolated HOME as DownloadMod, so the two agree on where state lives. Stdio
// is left unset so tea.ExecProcess can attach it to the terminal for the
// password and Steam Guard prompts; steamcmd then caches the credentials in
// its own install dir.
func LoginCmd(steamcmdPath, steamLogin string) (*exec.Cmd, error) {
	if err := ValidateSteamLogin(steamLogin); err != nil {
		return nil, err
	}
	return steamCMDCommand(steamcmdPath, "+login", steamLogin, "+quit")
}

// ValidateSteamLogin checks a Steam account name before it becomes a steamcmd
// argument. steamcmd reads +commands from its argument list and its
// interactive input, so anything but a plain account name could smuggle in
// a command of its own. Steam account names are letters, digits and '_'.
func ValidateSteamLogin(login string) error {
	if login == "" {
		return errors.New("the steam login is empty (use anonymous or your account name)")
	}
	for _, r := range login {
		if !isAccountNameRune(r) {
			return fmt.Errorf("invalid Steam login %q: Steam account names use only letters, digits and '_'", login)
		}
	}
	return nil
}

func isAccountNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_'
}

// ValidateContentDir checks steamcmd's install dir before it becomes a
// +force_install_dir argument. steamcmd may split its arguments again on
// whitespace and treat a '+' as the start of a command (not verified), and
// a '"' could end a quoted value, so a dir with any of them is refused.
func ValidateContentDir(dir string) error {
	if dir == "" {
		return errors.New("steamcmd content dir unknown")
	}
	if strings.ContainsFunc(dir, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '+' }) {
		return fmt.Errorf("steamcmd content dir %q contains whitespace, '\"' or '+', which steamcmd may misread; "+
			"use the Steamworks backend, or point XDG_DATA_HOME somewhere without them", dir)
	}
	return nil
}

// runError wraps a failed steamcmd run, tagging login failures with
// ErrLoginRequired. The full output is classified; only the tail is kept.
func runError(what string, err error, out []byte) error {
	if IsLoginFailure(out) {
		return fmt.Errorf("steamcmd %s: %w: %s", what, ErrLoginRequired, lastLines(out, 3))
	}
	return fmt.Errorf("steamcmd %s: %w: %s", what, err, lastLines(out, 3))
}

// downloadArgs is the steamcmd argument list for one workshop item.
// +force_install_dir must come before +login; steamcmd warns and may ignore
// it otherwise.
func downloadArgs(contentDir, steamLogin, modID string) []string {
	return []string{
		"+@ShutdownOnFailedCommand", "1",
		"+force_install_dir", contentDir,
		"+login", steamLogin,
		"+workshop_download_item", GameID, modID, "validate",
		"+quit",
	}
}

// DownloadMod downloads/validates a workshop mod via steamcmd into
// contentDir (Paths.CmdContent, dzman's steamcmd content dir) and marks it as
// managed. Output is captured (not streamed) so it cannot corrupt the TUI.
func DownloadMod(steamcmdPath, steamLogin, contentDir, modID string) error {
	return DownloadModContext(context.Background(), steamcmdPath, steamLogin, contentDir, modID)
}

// steamCMDStopGrace is how long a cancelled steamcmd group gets to exit after
// SIGTERM before it is killed. It is shorter than steamCMDWaitDelay, the time
// exec waits for the output pipe once the command is stopped.
const (
	steamCMDStopGrace = 8 * time.Second
	steamCMDWaitDelay = 10 * time.Second
)

// DownloadModContext is DownloadMod that stops steamcmd when ctx is
// cancelled: it gets SIGTERM, then a kill after steamCMDStopGrace. With a
// cancellable ctx steamcmd also gets SIGTERM if dzman itself dies. A ctx that
// can never be cancelled (context.Background) changes nothing.
func DownloadModContext(ctx context.Context, steamcmdPath, steamLogin, contentDir, modID string) error {
	if err := ValidateSteamLogin(steamLogin); err != nil {
		return err
	}
	if !ValidModID(modID) {
		return fmt.Errorf("invalid workshop ID %q", modID)
	}
	if err := ValidateContentDir(contentDir); err != nil {
		return err
	}
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return fmt.Errorf("create steamcmd content dir: %w", err)
	}
	cmd, finished, err := steamCMDCommandContext(ctx, steamcmdPath, downloadArgs(contentDir, steamLogin, modID)...)
	if err != nil {
		return err
	}
	defer finished()
	if ctx.Done() != nil {
		// Pdeathsig fires when the thread that started the process exits, so
		// keep this goroutine on one thread until steamcmd is done.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return runError("download mod "+modID, err, out)
	}
	workshop := WorkshopDir(contentDir)
	if _, ok := findModDir([]string{workshop}, modID); !ok {
		return fmt.Errorf("steamcmd download mod %s: finished but %s has no such item", modID, workshop)
	}
	return MarkManaged(workshop, modID)
}

// lastLines returns the last n non-empty lines of output for error context.
func lastLines(out []byte, n int) string {
	var lines []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
