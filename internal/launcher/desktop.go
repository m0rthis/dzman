package launcher

import (
	"fmt"
	"os"
	"path/filepath"
)

// AppsDir returns the user's XDG applications directory.
func AppsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "applications")
}

// ExecPath returns the real path of the running binary (resolves symlinks).
func ExecPath() string {
	path, err := os.Executable()
	if err != nil {
		return "dzman"
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return real
}

// DesktopFileName returns the .desktop filename for a server.
func DesktopFileName(ip string, gamePort int) string {
	return fmt.Sprintf("dzman-%s-%d.desktop", ip, gamePort)
}

// DesktopEntryExists reports whether a .desktop shortcut exists for the server.
func DesktopEntryExists(appsDir, ip string, gamePort int) bool {
	_, err := os.Stat(filepath.Join(appsDir, DesktopFileName(ip, gamePort)))
	return err == nil
}

// CreateDesktopEntry writes a .desktop file for quick-connecting to a server.
func CreateDesktopEntry(appsDir, serverName, ip string, gamePort int, execPath string) error {
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return fmt.Errorf("create apps dir: %w", err)
	}
	path := filepath.Join(appsDir, DesktopFileName(ip, gamePort))
	content := fmt.Sprintf(
		"[Desktop Entry]\nName=DayZ - %s\nComment=Play DayZ on %s\n"+
			"Exec=%s connect %s %d\nIcon=steam_icon_221100\n"+
			"Terminal=true\nType=Application\nCategories=Game;\n",
		serverName, serverName, execPath, ip, gamePort,
	)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write desktop entry: %w", err)
	}
	return nil
}

// DeleteDesktopEntry removes the .desktop file for a server.
func DeleteDesktopEntry(appsDir, ip string, gamePort int) error {
	path := filepath.Join(appsDir, DesktopFileName(ip, gamePort))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete desktop entry: %w", err)
	}
	return nil
}
