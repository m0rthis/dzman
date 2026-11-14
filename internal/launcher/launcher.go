// Package launcher builds DayZ launch arguments and manages game/Steam processes.
package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/m0rthis/dzman/internal/procs"
	"github.com/m0rthis/dzman/internal/steamworks"
)

const (
	// GameID is the Steam app ID for DayZ.
	GameID = "221100"
	// launchPollTimeout is how long we wait for DayZ to appear.
	launchPollTimeout = 90 * time.Second
)

// ConnectArgs holds everything needed to build a DayZ connection command.
type ConnectArgs struct {
	// ServerIP is the game server IP address.
	ServerIP string
	// GamePort is the DayZ game port.
	GamePort int
	// Password is the server password, empty if none.
	Password string
	// ModIDs are the Steam Workshop IDs to load (e.g. ["123", "456"]).
	ModIDs []string
	// PlayerName is the -name argument passed to DayZ.
	PlayerName string
	// ExtraArgs are the profile launch options.
	ExtraArgs []string
}

// ToSteamArgs converts ConnectArgs to the argument list passed after
// `steam -applaunch <appid> -nolauncher -name <player> ...`.
func (a ConnectArgs) ToSteamArgs() []string {
	var args []string

	if len(a.ModIDs) > 0 {
		modParts := make([]string, len(a.ModIDs))
		for i, id := range a.ModIDs {
			modParts[i] = "@" + id
		}
		args = append(args, "-mod="+strings.Join(modParts, ";"))
	}
	if a.ServerIP != "" {
		args = append(args,
			fmt.Sprintf("-connect=%s", a.ServerIP),
			fmt.Sprintf("-port=%d", a.GamePort),
		)
	}
	if a.Password != "" {
		args = append(args, fmt.Sprintf("-password=%s", a.Password))
	}
	args = append(args, a.ExtraArgs...)
	return args
}

// LaunchDayZ starts DayZ via Steam with the provided game args.
// It blocks until DayZ appears in the process list or the poll timeout fires.
func LaunchDayZ(playerName string, gameArgs []string) error {
	return LaunchDayZNotify(playerName, gameArgs, nil)
}

// LaunchDayZNotify is LaunchDayZ that calls started, if not nil, once the
// steam command has been started, before it polls for the game.
func LaunchDayZNotify(playerName string, gameArgs []string, started func()) error {
	if IsDayZRunning() {
		return fmt.Errorf("DayZ is already running")
	}
	// While the helper has the Steam API open as DayZ, the client counts the
	// game as running; launching now would fail or confuse cloud sync.
	if steamworks.IsHelperRunning(procs.ByArgv) {
		return fmt.Errorf("a Steamworks mod download is still running; wait for it to finish or cancel it")
	}

	steamArgs := append(
		[]string{"-applaunch", GameID, "-nolauncher", "-name", playerName},
		gameArgs...,
	)

	// Detach into its own session so Steam survives our exit; if the client
	// is already running this invocation only forwards the args and exits.
	cmd := exec.Command("steam", steamArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open devnull: %w", err)
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch DayZ: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	if started != nil {
		started()
	}

	// Wait for the DayZ process to appear.
	deadline := time.Now().Add(launchPollTimeout)
	for time.Now().Before(deadline) {
		if IsDayZRunning() {
			return nil
		}
		time.Sleep(time.Second)
	}
	return nil
}

// KillDayZ terminates all DayZ processes.
func KillDayZ() error {
	pids := procs.ByCmdline("DayZ", ".exe")
	if len(pids) == 0 {
		return nil
	}
	if procs.Signal(pids, syscall.SIGTERM) == 0 {
		return fmt.Errorf("kill DayZ: no process signalled")
	}
	return nil
}

// IsDayZRunning returns true if any DayZ process is active.
func IsDayZRunning() bool {
	return len(procs.ByCmdline("DayZ", ".exe")) > 0
}

// IsSteamRunning returns true if the Steam client is running.
func IsSteamRunning() bool {
	return len(procs.ByComm("steam")) > 0
}

// CheckSysctl checks whether vm.max_map_count meets DayZ's requirement.
// Returns (ok, error). If the file can't be read we assume ok=true.
func CheckSysctl() (bool, error) {
	const required = 1 << 20 // 1 048 576
	data, err := os.ReadFile("/proc/sys/vm/max_map_count")
	if err != nil {
		return true, nil // Can't check — assume fine.
	}
	var current int
	if _, err := fmt.Sscan(strings.TrimSpace(string(data)), &current); err != nil {
		return true, nil
	}
	return current >= required, nil
}

// SetSysctl sets vm.max_map_count to the required value via sudo.
func SetSysctl() error {
	const (
		required = 1 << 20
		confFile = "/etc/sysctl.d/50-dzman.conf"
	)
	content := fmt.Sprintf("vm.max_map_count=%d\n", required)

	tee := exec.Command("sudo", "tee", confFile)
	tee.Stdin = strings.NewReader(content)
	if out, err := tee.CombinedOutput(); err != nil {
		return fmt.Errorf("write sysctl conf: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	apply := exec.Command("sudo", "sysctl", "-w",
		fmt.Sprintf("vm.max_map_count=%d", required))
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("apply sysctl: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
