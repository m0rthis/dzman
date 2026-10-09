package steam

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultSteamCMD is the stored SteamCMDPath value that means "auto-detect".
const DefaultSteamCMD = "steamcmd"

// homeSteamCMDPaths are the usual per-user install spots, relative to $HOME.
// Valve's tarball ships steamcmd.sh, typically unpacked into ~/steamcmd.
var homeSteamCMDPaths = []string{
	"steamcmd/steamcmd.sh",
	"Steam/steamcmd.sh",
	".steam/steamcmd/steamcmd.sh",
	".local/share/Steam/steamcmd/steamcmd.sh",
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
func FindSteamCMD(configured string) (string, error) {
	home, _ := os.UserHomeDir()
	return findSteamCMD(configured, home, exec.LookPath)
}

func findSteamCMD(configured, home string, lookPath func(string) (string, error)) (string, error) {
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

	for _, name := range []string{"steamcmd", "steamcmd.sh"} {
		if found, err := lookPath(name); err == nil {
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
		if isExecutableFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("steamcmd not found (looked for steamcmd and steamcmd.sh on PATH, in %s); "+
		"install it or set its path under Config > Change SteamCMD Path", strings.Join(tried, ", "))
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
