package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/m0rthis/dzman/internal/config"
	"github.com/m0rthis/dzman/internal/launcher"
	"github.com/m0rthis/dzman/internal/steam"
	"github.com/m0rthis/dzman/internal/steamworks"
	"github.com/m0rthis/dzman/internal/tui"
	"github.com/m0rthis/dzman/internal/version"
)

func main() {
	// Hidden: the short-lived Steamworks download helper the TUI re-execs.
	if len(os.Args) > 1 && os.Args[1] == steamworks.HelperCommand {
		os.Exit(steamworks.RunHelper(os.Args[2:]))
	}

	// Handle `dzman connect <ip> <port>` sub-command for desktop-entry shortcuts.
	if len(os.Args) == 4 && os.Args[1] == "connect" {
		directConnect(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "rejoin" {
		rejoin()
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
	_, err = p.Run()
	// Never leave a helper running: the Steam client would think DayZ is.
	if steamworks.Active() {
		fmt.Fprintln(os.Stderr, "stopping Steamworks helper…")
	}
	steamworks.CancelAll()
	if err != nil {
		fatalf("run: %v", err)
	}
}

// directConnect is called when the binary is invoked as `dzman connect ip port`.
// It syncs the server's mods, records the join and launches the game.
func directConnect(ipArg, portArg string) {
	port, err := strconv.Atoi(portArg)
	if err != nil {
		cliFail("invalid port %q: %v", portArg, err)
	}
	cfg, profile, paths := loadState()
	join(cfg, profile, paths, tui.JoinTarget{IP: ipArg, Port: port, ByGamePort: true, Name: fmt.Sprintf("%s:%d", ipArg, port)})
}

// rejoin is `dzman rejoin`: join the most recent server in the history.
func rejoin() {
	cfg, profile, paths := loadState()
	t, err := tui.RejoinTarget(profile)
	if err != nil {
		cliFail("%v", err)
	}
	join(cfg, profile, paths, t)
}

// loadState loads the config and profile and detects the Steam paths, as the
// TUI start-up does. Missing paths are not an error here; Join reports them
// when it needs them.
func loadState() (*config.Config, *config.Profile, *steam.Paths) {
	cfg, err := config.Load()
	if err != nil {
		cliFail("load config: %v", err)
	}
	profile, err := config.LoadProfile(cfg.ProfilePath)
	if err != nil {
		cliFail("load profile: %v", err)
	}
	paths, _ := steam.DetectPaths(profile.SteamRoot)
	return cfg, profile, paths
}

// join runs the shared terminal join and exits non-zero if it fails.
func join(cfg *config.Config, profile *config.Profile, paths *steam.Paths, t tui.JoinTarget) {
	password := os.Getenv(tui.PasswordEnv)
	// The helper, steamcmd and Steam must not inherit the password.
	_ = os.Unsetenv(tui.PasswordEnv)
	err := tui.Join(tui.JoinOptions{
		Cfg: cfg, Profile: profile, Paths: paths, Target: t,
		Password: password, Out: os.Stdout,
	})
	// Never leave a helper running, whatever happened.
	steamworks.CancelAll()
	if errors.Is(err, tui.ErrInterrupted) {
		fmt.Fprintln(os.Stderr, "dzman: interrupted")
		os.Exit(130)
	}
	if err != nil {
		cliFail("%v", err)
	}
}

// cliFail is fatalf for connect and rejoin. Desktop shortcuts run in a
// terminal that closes on exit, so when someone is there to read it, it waits
// for Enter. The message may carry server text, so control characters are
// dropped.
func cliFail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dzman: %s\n", tui.SanitizeText(fmt.Sprintf(format, args...)))
	if canPrompt() {
		fmt.Fprint(os.Stderr, "Press Enter to close…")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
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
  dzman connect <ip> <port>  sync the server's mods, then connect (<port> is the game port)
  dzman rejoin               do the same for the most recent server in the history
  dzman version              print version information
  dzman help                 show this help

connect and rejoin print progress instead of opening the TUI, and do not launch
DayZ if the mods cannot be installed. A server password is read from DZMAN_PASSWORD.`)
}
