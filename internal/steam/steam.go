// Package steam provides Steam and SteamCMD integration: path detection,
// installed-mod scanning, symlink management, and client control.
package steam

import (
	"bufio"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/braydencw1/dzman/internal/procs"
)

// GameID is the Steam App ID for DayZ.
const GameID = "221100"

// Paths holds the resolved Steam and DayZ directory paths.
type Paths struct {
	// SteamApps is the steamapps directory (e.g. ~/.steam/steam/steamapps).
	SteamApps string
	// DayZ is the game directory (SteamApps/common/DayZ).
	DayZ string
	// Workshop is the DayZ workshop content directory.
	Workshop string
}

// DetectPaths auto-detects Steam paths, falling back to customRoot if set.
func DetectPaths(customRoot string) (*Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	candidates := []string{
		filepath.Join(home, ".steam", "steam", "steamapps"),
		filepath.Join(home, ".local", "share", "Steam", "steamapps"),
		filepath.Join(home, ".var", "app", "com.valvesoftware.Steam", "data", "Steam", "steamapps"),
	}
	if customRoot != "" {
		candidates = append([]string{filepath.Join(customRoot, "steamapps")}, candidates...)
	}

	for _, root := range candidates {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		dayzPath := filepath.Join(root, "common", "DayZ")
		if _, err := os.Stat(dayzPath); err != nil {
			continue
		}
		return &Paths{
			SteamApps: root,
			DayZ:      dayzPath,
			Workshop:  filepath.Join(root, "workshop", "content", GameID),
		}, nil
	}
	return nil, fmt.Errorf("DayZ not found; specify steam root in config")
}

// InstalledMod is a mod found in the Workshop content directory.
type InstalledMod struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	// Size is the directory size in bytes.
	Size    int64 `json:"size"`
	Managed bool  `json:"managed"` // installed by dzman (has .dzman marker file)
}

var (
	reName = regexp.MustCompile(`name\s*=\s*"([^"]*)"`)
	reID   = regexp.MustCompile(`publishedid\s*=\s*(\d+)`)
	reTS   = regexp.MustCompile(`timestamp\s*=\s*(-?\d+)`)
)

// ScanMods reads the workshop directory and returns all installed DayZ mods.
func ScanMods(workshopPath string) ([]InstalledMod, error) {
	var mods []InstalledMod

	entries, err := os.ReadDir(workshopPath)
	if err != nil {
		if os.IsNotExist(err) {
			return mods, nil
		}
		return nil, fmt.Errorf("read workshop dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		modPath := filepath.Join(workshopPath, e.Name())
		metaPath := filepath.Join(modPath, "meta.cpp")
		if _, err := os.Stat(metaPath); err != nil {
			continue
		}

		name, id, ts, err := parseMetaCpp(metaPath)
		if err != nil {
			continue
		}

		size, err := dirSize(modPath)
		if err != nil {
			size = 0
		}

		managed := false
		if _, err := os.Stat(filepath.Join(modPath, ".dzman")); err == nil {
			managed = true
		}

		mods = append(mods, InstalledMod{
			Name:      name,
			ID:        id,
			Timestamp: ts,
			Size:      size,
			Managed:   managed,
		})
	}

	sort.Slice(mods, func(i, j int) bool {
		return mods[i].Size > mods[j].Size
	})
	return mods, nil
}

// WorkshopChecksum computes an MD5 over the sorted directory names in workshopPath.
// This is used to detect when the mod set has changed.
func WorkshopChecksum(workshopPath string) (string, error) {
	entries, err := os.ReadDir(workshopPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read workshop dir: %w", err)
	}

	h := md5.New()
	for _, e := range entries {
		if e.IsDir() {
			if _, err := io.WriteString(h, e.Name()+"/"); err != nil {
				return "", fmt.Errorf("hash entry: %w", err)
			}
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// EnsureSymlinks creates @ID symlinks in dayzPath pointing to each mod in
// workshopPath. It fails if a mod directory does not actually exist, so we
// never hand DayZ a -mod= list of broken links.
func EnsureSymlinks(dayzPath, workshopPath string, modIDs []string) error {
	for _, id := range modIDs {
		src := filepath.Join(workshopPath, id)
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			return fmt.Errorf("mod %s is not installed in %s", id, workshopPath)
		}
		linkPath := filepath.Join(dayzPath, "@"+id)
		if _, err := os.Lstat(linkPath); err == nil {
			continue // already exists
		}
		if err := os.Symlink(src, linkPath); err != nil {
			return fmt.Errorf("symlink mod %s: %w", id, err)
		}
	}
	return nil
}

// RemoveSymlinks deletes all @ID symlinks from dayzPath and returns the count.
func RemoveSymlinks(dayzPath string) (int, error) {
	entries, err := os.ReadDir(dayzPath)
	if err != nil {
		return 0, fmt.Errorf("read dayz dir: %w", err)
	}
	count := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "@") {
			continue
		}
		linkPath := filepath.Join(dayzPath, e.Name())
		info, err := os.Lstat(linkPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(linkPath); err != nil {
				return count, fmt.Errorf("remove link %s: %w", linkPath, err)
			}
			count++
		}
	}
	return count, nil
}

// MarkManaged writes a .dzman marker file inside the mod directory.
func MarkManaged(workshopPath, id string) error {
	marker := filepath.Join(workshopPath, id, ".dzman")
	if err := os.WriteFile(marker, []byte(id), 0o644); err != nil {
		return fmt.Errorf("mark managed %s: %w", id, err)
	}
	return nil
}

// RemoveManagedMods deletes all mods that have a .dzman marker. Returns the count.
func RemoveManagedMods(workshopPath string) (int, error) {
	entries, err := os.ReadDir(workshopPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read workshop dir: %w", err)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		marker := filepath.Join(workshopPath, e.Name(), ".dzman")
		if _, err := os.Stat(marker); err != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(workshopPath, e.Name())); err != nil {
			return count, fmt.Errorf("remove mod %s: %w", e.Name(), err)
		}
		count++
	}
	return count, nil
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

// LoginCmd builds an interactive `steamcmd +login <name> +quit`. Stdio is left
// unset so tea.ExecProcess can attach it to the terminal for the password and
// Steam Guard prompts; steamcmd then caches the credentials.
func LoginCmd(steamcmdPath, steamLogin string) *exec.Cmd {
	return exec.Command(steamcmdPath, "+login", steamLogin, "+quit")
}

// runError wraps a failed steamcmd run, tagging login failures with
// ErrLoginRequired. The full output is classified; only the tail is kept.
func runError(what string, err error, out []byte) error {
	if IsLoginFailure(out) {
		return fmt.Errorf("steamcmd %s: %w: %s", what, ErrLoginRequired, lastLines(out, 3))
	}
	return fmt.Errorf("steamcmd %s: %w: %s", what, err, lastLines(out, 3))
}

// DownloadMod downloads/validates a workshop mod via steamcmd.
// Output is captured (not streamed) so it cannot corrupt the TUI.
func DownloadMod(steamcmdPath, steamLogin, modID string) error {
	cmd := exec.Command(steamcmdPath,
		"+@ShutdownOnFailedCommand", "1",
		"+login", steamLogin,
		"+workshop_download_item", GameID, modID, "validate",
		"+quit",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return runError("download mod "+modID, err, out)
	}
	return nil
}

// UpdateGame forces a DayZ game update via steamcmd.
func UpdateGame(steamcmdPath, steamLogin string) error {
	cmd := exec.Command(steamcmdPath,
		"+@ShutdownOnFailedCommand", "1",
		"+login", steamLogin,
		"+app_update", GameID,
		"+quit",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return runError("update game", err, out)
	}
	return nil
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

// IsSteamRunning returns true if the Steam client process is running.
// Matches the exact comm "steam", so steamcmd does not count.
func IsSteamRunning() bool {
	return len(procs.ByComm("steam")) > 0
}

// StartSteam launches the Steam client silently and waits briefly.
func StartSteam() error {
	cmd := exec.Command("steam", "-nofriendsui", "-silent")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start steam: %w", err)
	}
	time.Sleep(5 * time.Second)
	return nil
}

// ShutdownSteam asks Steam to exit gracefully and waits for the process to
// disappear, escalating to SIGTERM and finally SIGKILL if it lingers.
func ShutdownSteam() error {
	if !IsSteamRunning() {
		return nil
	}
	// `steam -shutdown` returns immediately; the client exits asynchronously.
	_ = exec.Command("steam", "-shutdown").Run()
	if waitSteamGone(30 * time.Second) {
		return nil
	}
	procs.Signal(procs.ByComm("steam"), syscall.SIGTERM)
	if waitSteamGone(10 * time.Second) {
		return nil
	}
	procs.Signal(procs.ByComm("steam"), syscall.SIGKILL)
	if waitSteamGone(5 * time.Second) {
		return nil
	}
	return fmt.Errorf("shutdown steam: client still running")
}

// waitSteamGone polls until the Steam client exits or the timeout elapses.
func waitSteamGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !IsSteamRunning() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// --- helpers ---

func parseMetaCpp(path string) (name, id string, ts int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := reName.FindStringSubmatch(line); m != nil {
			name = m[1]
		}
		if m := reID.FindStringSubmatch(line); m != nil {
			id = m[1]
		}
		if m := reTS.FindStringSubmatch(line); m != nil {
			ts, _ = strconv.ParseInt(m[1], 10, 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", 0, err
	}
	if name == "" || id == "" {
		return "", "", 0, fmt.Errorf("incomplete meta.cpp in %s", path)
	}
	return name, id, ts, nil
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
