// Package steam provides Steam and SteamCMD integration: path detection,
// installed-mod scanning, symlink management, and client control.
package steam

import (
	"bufio"
	"crypto/md5"
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

	"github.com/m0rthis/dzman/internal/procs"
)

// GameID is the Steam App ID for DayZ.
const GameID = "221100"

// Paths holds the resolved Steam and DayZ directory paths.
type Paths struct {
	// SteamApps is the steamapps directory (e.g. ~/.steam/steam/steamapps).
	SteamApps string
	// DayZ is the game directory (SteamApps/common/DayZ).
	DayZ string
	// Workshop is the Steam client's DayZ workshop content directory. The
	// client owns everything in it; dzman never writes or deletes there.
	Workshop string
	// CmdContent is dzman's steamcmd install dir (see SteamCMDContentDir).
	// Empty if unknown.
	CmdContent string
	// CmdWorkshop is the DayZ workshop content directory inside CmdContent.
	// Empty if unknown.
	CmdWorkshop string
	// Client is the Steam client's own root (see FindClientRoot). It differs
	// from Root when DayZ is in a secondary library. Empty if unknown.
	Client string
}

// Root returns the root of the Steam library DayZ is in (the parent of
// SteamApps). That is the client's root only when DayZ is in the client's
// main library; use ClientRoot for the client.
func (p *Paths) Root() string { return filepath.Dir(p.SteamApps) }

// ClientRoot returns the Steam client's root: Client when known, else Root.
func (p *Paths) ClientRoot() string {
	if p.Client != "" {
		return p.Client
	}
	return p.Root()
}

// Flatpak reports whether the Steam client is Flatpak Steam: its root, or
// the library DayZ is in, is inside Flatpak Steam's tree.
func (p *Paths) Flatpak() bool {
	return IsFlatpakClient(p.ClientRoot()) || IsFlatpakClient(p.Root())
}

// ModDirs returns the directories searched for workshop mods, in lookup
// order: the Steam client's workshop dir first, then dzman's steamcmd dir.
func (p *Paths) ModDirs() []string {
	dirs := []string{p.Workshop}
	if p.CmdWorkshop != "" && p.CmdWorkshop != p.Workshop {
		dirs = append(dirs, p.CmdWorkshop)
	}
	return dirs
}

// FindMod returns the directory of an installed mod, searching ModDirs in
// order. ok is false if the mod is in none of them.
func (p *Paths) FindMod(id string) (dir string, ok bool) {
	return findModDir(p.ModDirs(), id)
}

func findModDir(dirs []string, id string) (string, bool) {
	if !ValidModID(id) {
		return "", false
	}
	for _, d := range dirs {
		src := filepath.Join(d, id)
		if info, err := os.Stat(src); err == nil && info.IsDir() {
			return src, true
		}
	}
	return "", false
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
		p := &Paths{
			SteamApps: root,
			DayZ:      dayzPath,
			Workshop:  WorkshopDir(filepath.Dir(root)),
			Client:    findClientRoot(home, customRoot),
		}
		if content, err := SteamCMDContentDir(p.Flatpak()); err == nil {
			p.CmdContent, p.CmdWorkshop = content, WorkshopDir(content)
		}
		return p, nil
	}
	return nil, fmt.Errorf("DayZ not found; specify steam root in config")
}

// InstalledMod is a mod found in a workshop content directory.
type InstalledMod struct {
	Name string `json:"name"`
	// ID is the workshop ID, taken from the mod's folder name, which is what
	// Steam, steamcmd, FindMod and the @ID links all go by.
	ID string `json:"id"`
	// MetaID is meta.cpp's publishedid, for display only. It can be missing,
	// 0, or differ from ID (e.g. a mod re-uploaded under a new item).
	MetaID    string `json:"meta_id,omitempty"`
	Timestamp int64  `json:"timestamp"`
	// Size is the directory size in bytes.
	Size int64 `json:"size"`
	// Dir is the mod's directory.
	Dir string `json:"dir"`
	// Managed is true for mods in dzman's steamcmd dir that carry a .dzman
	// marker. Mods in the Steam client's workshop dir are never managed.
	Managed bool `json:"managed"`
}

var (
	reName = regexp.MustCompile(`name\s*=\s*"([^"]*)"`)
	reID   = regexp.MustCompile(`publishedid\s*=\s*(\d+)`)
	reTS   = regexp.MustCompile(`timestamp\s*=\s*(-?\d+)`)
)

// ScanAllMods scans every directory in p.ModDirs. A mod present in more than
// one is reported once, from the first directory (the Steam client's).
func ScanAllMods(p *Paths) ([]InstalledMod, error) {
	var all []InstalledMod
	seen := map[string]bool{}
	for i, dir := range p.ModDirs() {
		mods, err := scanDir(dir, i > 0)
		if err != nil {
			return nil, err
		}
		for _, m := range mods {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Size > all[j].Size
	})
	return all, nil
}

// ScanMods reads one workshop directory and returns the DayZ mods in it.
// Markers are honoured, so only use it on dzman's steamcmd dir or in tests.
func ScanMods(workshopPath string) ([]InstalledMod, error) {
	mods, err := scanDir(workshopPath, true)
	if err != nil {
		return nil, err
	}
	sort.Slice(mods, func(i, j int) bool {
		return mods[i].Size > mods[j].Size
	})
	return mods, nil
}

// scanDir lists the mods in workshopPath. honourMarkers controls whether a
// .dzman marker makes a mod Managed.
func scanDir(workshopPath string, honourMarkers bool) ([]InstalledMod, error) {
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
		if !ValidModID(e.Name()) {
			continue // not a workshop item folder
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
		if honourMarkers {
			if _, err := os.Stat(filepath.Join(modPath, ".dzman")); err == nil {
				managed = true
			}
		}

		mods = append(mods, InstalledMod{
			Name:      name,
			ID:        e.Name(),
			MetaID:    id,
			Timestamp: ts,
			Size:      size,
			Dir:       modPath,
			Managed:   managed,
		})
	}
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

// ValidModID reports whether id is a workshop item ID in canonical form:
// decimal digits only, no leading zero, nonzero, and fitting in a uint64.
// Anything else (-1, 1.5, 1e5, 0123, a path) is rejected before it reaches
// Steam, steamcmd or the file system, so an ID always matches the helper's
// strconv.FormatUint form and the client's folder name.
func ValidModID(id string) bool {
	if id == "" || id[0] == '0' {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n != 0
}

// InvalidModIDs returns the IDs that fail ValidModID, in order.
func InvalidModIDs(ids []string) []string {
	var bad []string
	for _, id := range ids {
		if !ValidModID(id) {
			bad = append(bad, id)
		}
	}
	return bad
}

// EnsureSymlinks makes an @ID symlink in dayzPath for each mod, pointing at
// the first of modDirs that has it. An existing @ID symlink that points
// anywhere else (a stale or missing source) is replaced: dzman owns the @ID
// links in the DayZ folder. A real file or
// directory named @ID is left alone. It fails if any ID is not a valid
// workshop ID or a mod is in none of modDirs, so we never hand DayZ a -mod=
// list of broken links.
func EnsureSymlinks(dayzPath string, modDirs []string, modIDs []string) error {
	if bad := InvalidModIDs(modIDs); len(bad) > 0 {
		return fmt.Errorf("invalid workshop IDs from the server: %q", bad)
	}
	for _, id := range modIDs {
		src, ok := findModDir(modDirs, id)
		if !ok {
			return fmt.Errorf("mod %s is not installed in %s", id, strings.Join(modDirs, " or "))
		}
		linkPath := filepath.Join(dayzPath, "@"+id)
		info, err := os.Lstat(linkPath)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			return fmt.Errorf("check link %s: %w", linkPath, err)
		case info.Mode()&os.ModeSymlink == 0:
			continue // a real mod dir someone put there; not ours to touch
		default:
			if target, err := os.Readlink(linkPath); err == nil && filepath.Clean(target) == src {
				continue
			}
			if err := os.Remove(linkPath); err != nil {
				return fmt.Errorf("replace stale link %s: %w", linkPath, err)
			}
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

// MarkManaged writes a .dzman marker file inside the mod directory. Only call
// it for dzman's steamcmd dir (Paths.CmdWorkshop), never the client's.
func MarkManaged(workshopPath, id string) error {
	marker := filepath.Join(workshopPath, id, ".dzman")
	if err := os.WriteFile(marker, []byte(id), 0o644); err != nil {
		return fmt.Errorf("mark managed %s: %w", id, err)
	}
	return nil
}

// RemoveManagedMods deletes all mods that have a .dzman marker from dzman's
// steamcmd dir and returns the count. The Steam client's workshop dir is
// never touched, even if old dzman versions left markers there.
func RemoveManagedMods(p *Paths) (int, error) {
	if p.CmdWorkshop == "" {
		return 0, nil
	}
	return removeMarked(p.CmdWorkshop, ClientRoots(p.Root(), p.ClientRoot()))
}

// removeMarked deletes the marked mods in workshopPath. It refuses outright
// if workshopPath resolves to somewhere inside a Steam client root, since
// the client's mods are never dzman's to delete.
func removeMarked(workshopPath string, clientRoots []string) (int, error) {
	resolved := resolvePath(workshopPath)
	for _, root := range clientRoots {
		if root != "" && isWithin(resolved, resolvePath(root)) {
			return 0, fmt.Errorf("refusing to remove mods: %s is inside the Steam client's folder %s", resolved, root)
		}
	}
	workshopPath = resolved
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
	if name == "" {
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
