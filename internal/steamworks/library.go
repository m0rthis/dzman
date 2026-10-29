package steamworks

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// LibName is the native Linux Steamworks library.
const LibName = "libsteam_api.so"

// ErrLibraryNotFound means no usable libsteam_api.so was found.
var ErrLibraryNotFound = errors.New("libsteam_api.so not found")

// libSubdirs are the places inside a game folder (steamapps/common/<game>)
// where native Linux games usually keep libsteam_api.so. Globs are allowed.
var libSubdirs = []string{
	"",
	"bin",
	"bin/linux64",
	"bin/x86_64",
	"linux64",
	"x86_64",
	"lib",
	"lib64",
	"lib/x86_64",
	"*_Data/Plugins",        // Unity
	"*_Data/Plugins/x86_64", // Unity
}

// FindOptions says where FindLibrary looks.
type FindOptions struct {
	// Configured is the steam_api_path setting. If set it must exist.
	Configured string
	// ConfigDir is dzman's config dir; ConfigDir/libsteam_api.so is tried next.
	ConfigDir string
	// SteamApps are steamapps dirs whose common/*/ game folders are scanned.
	SteamApps []string
}

// FindLibrary returns the path of a usable 64-bit libsteam_api.so. Order:
//
//  1. opts.Configured (an error if set but missing or unusable)
//  2. <ConfigDir>/libsteam_api.so (an error if present but unusable)
//  3. a scan of every steamapps/common/*/ game folder: of the usable copies
//     that are not likely Steam emulators, the one with the newest ISteamUGC
//     accessor, the first in sorted path order on a tie
//
// The returned path is absolute, so the helper dlopens exactly the file
// that was inspected.
func FindLibrary(opts FindOptions) (string, error) {
	return findLibrary(opts, checkLibrary)
}

// maxUGCAccessors is how many distinct SteamAPI_SteamUGC_vNNN accessors a
// scanned library may export. The real library exports the current one and,
// at most, a few older ones. Steam emulators (Goldberg, gbe_fork) export
// every version so any game binds, and must not be picked: they would
// pretend to download. So they are filtered out before the scan ranks the
// rest by version; ranking alone would favour exactly them.
const maxUGCAccessors = 3

// checkFunc validates a candidate and says what it exports.
type checkFunc func(path string) (libInfo, error)

func findLibrary(opts FindOptions, check checkFunc) (string, error) {
	if p := expandHome(strings.TrimSpace(opts.Configured)); p != "" {
		if !filepath.IsAbs(p) && opts.ConfigDir != "" {
			p = filepath.Join(opts.ConfigDir, p) // relative to dzman's config dir
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("steam_api_path %s: %w", p, err)
		}
		p = abs
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("steam_api_path %s: %w", p, err)
		}
		if _, err := check(p); err != nil {
			return "", fmt.Errorf("steam_api_path %s: %w", p, err)
		}
		return p, nil
	}
	if opts.ConfigDir != "" {
		p := filepath.Join(opts.ConfigDir, LibName)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if _, err := os.Stat(p); err == nil {
			if _, err := check(p); err != nil {
				return "", fmt.Errorf("%s: %w", p, err)
			}
			return p, nil
		}
	}

	best, bestVersion := "", -1
	for _, cand := range scanCandidates(opts.SteamApps) {
		// Candidates come sorted, so a strict > keeps the first on a tie.
		if info, err := check(cand); err == nil && info.ugcCount <= maxUGCAccessors && info.ugcVersion > bestVersion {
			best, bestVersion = cand, info.ugcVersion
		}
	}
	if best != "" {
		if abs, err := filepath.Abs(best); err == nil {
			best = abs
		}
		return best, nil
	}
	where := "~/.config/dzman/"
	if opts.ConfigDir != "" {
		where = opts.ConfigDir + "/"
	}
	return "", fmt.Errorf("%w: no installed native Linux game ships a usable 64-bit copy. "+
		"Get redistributable_bin/linux64/libsteam_api.so from the Steamworks SDK "+
		"(partner.steamgames.com/downloads) and copy it to %s, or set steam_api_path", ErrLibraryNotFound, where)
}

// scanCandidates lists every libsteam_api.so under the game folders, in a
// stable (sorted) order. Nothing is validated here.
func scanCandidates(steamApps []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, sa := range steamApps {
		games, err := os.ReadDir(filepath.Join(sa, "common"))
		if err != nil {
			continue
		}
		for _, g := range games {
			// Follow a symlinked game folder (a game moved to another disk).
			if fi, err := os.Stat(filepath.Join(sa, "common", g.Name())); err != nil || !fi.IsDir() {
				continue
			}
			// The game folder's name is literal; only libSubdirs are patterns.
			gameDir := filepath.Join(sa, "common", g.Name())
			for _, sub := range libSubdirs {
				matches, _ := filepath.Glob(filepath.Join(escapeGlob(gameDir), sub, LibName))
				sort.Strings(matches)
				for _, m := range matches {
					if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && !seen[m] {
						seen[m] = true
						out = append(out, m)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// escapeGlob escapes the filepath.Match metacharacters in a literal path.
func escapeGlob(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// checkLibrary is inspectLibrary as a checkFunc.
func checkLibrary(path string) (libInfo, error) { return inspectLibrary(path) }

// libInfo is what inspectLibrary learns about a usable library.
type libInfo struct {
	ugcVersion int    // newest ISteamUGC accessor version
	ugcSymbol  string // its exported name, e.g. SteamAPI_SteamUGC_v021
	ugcCount   int    // distinct SteamUGC accessor versions exported
}

// inspectLibrary opens path as an ELF file and checks that it matches this
// process (64-bit, same machine) and exports the flat API dzman calls.
func inspectLibrary(path string) (libInfo, error) {
	f, err := elf.Open(path)
	if err != nil {
		return libInfo{}, fmt.Errorf("not an ELF library: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := checkELFHeader(f.Class, f.Machine); err != nil {
		return libInfo{}, err
	}
	syms, err := f.DynamicSymbols()
	if err != nil {
		return libInfo{}, fmt.Errorf("read symbols: %w", err)
	}
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = s.Name
	}
	return checkSymbols(names)
}

// checkELFHeader rejects 32-bit libraries and ones built for another CPU.
func checkELFHeader(class elf.Class, machine elf.Machine) error {
	if class != elf.ELFCLASS64 {
		return fmt.Errorf("%s is not 64-bit", class)
	}
	if want, ok := goarchMachine[runtime.GOARCH]; ok && machine != want {
		return fmt.Errorf("built for %s, not %s", machine, want)
	}
	return nil
}

var goarchMachine = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

var reUGCAccessor = regexp.MustCompile(`^` + symUGCPrefix + `(\d+)$`)

// checkSymbols checks that the exported names cover what api.go loads and
// picks the newest ISteamUGC accessor, which loadAPI then binds by name.
func checkSymbols(names []string) (libInfo, error) {
	have := make(map[string]bool, len(names))
	info := libInfo{ugcVersion: -1}
	for _, n := range names {
		if have[n] {
			continue
		}
		have[n] = true
		if m := reUGCAccessor.FindStringSubmatch(n); m != nil {
			info.ugcCount++
			if v, err := strconv.Atoi(m[1]); err == nil && v > info.ugcVersion {
				info.ugcVersion, info.ugcSymbol = v, n
			}
		}
	}
	if !have[symInitFlat] && !have[symInit] {
		return libInfo{}, errors.New("exports neither SteamAPI_InitFlat nor SteamAPI_Init")
	}
	if info.ugcSymbol == "" {
		return libInfo{}, errors.New("no SteamAPI_SteamUGC_vNNN accessor (Steamworks SDK too old, need 1.48 or newer)")
	}
	for _, s := range requiredSymbols {
		if !have[s] {
			return libInfo{}, fmt.Errorf("missing %s", s)
		}
	}
	return info, nil
}

// LibraryFolders returns every steamapps dir known to the Steam client at
// steamRoot: steamRoot/steamapps plus the library folders listed in
// libraryfolders.vdf. Missing files are ignored.
func LibraryFolders(steamRoot string) []string {
	out := []string{filepath.Join(steamRoot, "steamapps")}
	seen := map[string]bool{filepath.Clean(out[0]): true}
	for _, vdf := range []string{
		filepath.Join(steamRoot, "steamapps", "libraryfolders.vdf"),
		filepath.Join(steamRoot, "config", "libraryfolders.vdf"),
	} {
		data, err := os.ReadFile(vdf)
		if err != nil {
			continue
		}
		for _, lib := range parseLibraryFolders(string(data)) {
			sa := filepath.Clean(filepath.Join(lib, "steamapps"))
			if !seen[sa] {
				seen[sa] = true
				out = append(out, sa)
			}
		}
	}
	return out
}

var reVDFPath = regexp.MustCompile(`"path"\s+"((?:[^"\\]|\\.)*)"`)

// parseLibraryFolders extracts the "path" values from libraryfolders.vdf.
func parseLibraryFolders(vdf string) []string {
	var out []string
	for _, m := range reVDFPath.FindAllStringSubmatch(vdf, -1) {
		p := strings.ReplaceAll(m[1], `\\`, `\`)
		if filepath.IsAbs(p) {
			out = append(out, p)
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
