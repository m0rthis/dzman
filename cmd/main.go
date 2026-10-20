package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/braydencw1/dzman/internal/config"
	"github.com/braydencw1/dzman/internal/launcher"
	"github.com/braydencw1/dzman/internal/steam"
	"github.com/braydencw1/dzman/internal/tui"
	"github.com/braydencw1/dzman/internal/version"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// Handle `dzman connect <ip> <port>` sub-command for desktop-entry shortcuts.
	if len(os.Args) == 4 && os.Args[1] == "connect" {
		directConnect(os.Args[2], os.Args[3])
		return
	}

	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			printVersion()
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fatalf("load config: %v", err)
	}

	profile, err := config.LoadProfile(cfg.ProfilePath)
	if err != nil {
		fatalf("load profile: %v", err)
	}

	// Try to auto-detect Steam paths (non-fatal; setup wizard handles missing paths).
	paths, _ := steam.DetectPaths(profile.SteamRoot)

	// Check vm.max_map_count (non-fatal advisory).
	if ok, err := launcher.CheckSysctl(); err == nil && !ok {
		fmt.Fprintln(os.Stderr,
			"Warning: vm.max_map_count is below 1048576. DayZ may not run correctly.")
		fmt.Fprintln(os.Stderr,
			"Run: echo 'vm.max_map_count=1048576' | sudo tee /etc/sysctl.d/50-dzman.conf && sudo sysctl -p")
	}

	tui.SetTheme(cfg.Theme)
	m := tui.New(cfg, profile, paths)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatalf("run: %v", err)
	}
}

// directConnect is called when the binary is invoked as `dzman connect ip port`.
// It looks up the server in the loaded list and launches the game.
func directConnect(ipArg, portArg string) {
	port, err := strconv.Atoi(portArg)
	if err != nil {
		fatalf("invalid port %q: %v", portArg, err)
	}

	cfg, err := config.Load()
	if err != nil {
		fatalf("load config: %v", err)
	}

	profile, err := config.LoadProfile(cfg.ProfilePath)
	if err != nil {
		fatalf("load profile: %v", err)
	}

	args := launcher.ConnectArgs{
		ServerIP:   ipArg,
		GamePort:   port,
		PlayerName: profile.Player,
		ExtraArgs:  profile.LaunchArgs(),
	}
	if err := launcher.LaunchDayZ(profile.Player, args.ToSteamArgs()); err != nil {
		fatalf("launch: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dzman: "+format+"\n", args...)
	os.Exit(1)
}

func printVersion() {
	fmt.Print(version.Get().String())
}

func printUsage() {
	fmt.Println(`dzman — DayZ launcher and server browser (TUI)

Usage:
  dzman                      launch the TUI
  dzman connect <ip> <port>  connect directly to a server
  dzman version              print version information
  dzman help                 show this help`)
}
