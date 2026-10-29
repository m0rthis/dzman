// Package procs inspects /proc to find and signal processes without
// shelling out to pgrep/pkill/killall.
package procs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ByComm returns the PIDs whose comm (executable name) equals name exactly.
// Unlike `pgrep name`, this does not match substrings, so "steam" will not
// match "steamcmd" or "steamwebhelper".
func ByComm(name string) []int {
	return scan(func(pidDir string) bool {
		data, err := os.ReadFile(filepath.Join(pidDir, "comm"))
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(data)) == name
	})
}

// ByCmdline returns the PIDs whose full command line contains every substring.
func ByCmdline(substrs ...string) []int {
	self := os.Getpid()
	return scan(func(pidDir string) bool {
		pid, _ := strconv.Atoi(filepath.Base(pidDir))
		if pid == self {
			return false
		}
		data, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
		if err != nil || len(data) == 0 {
			return false
		}
		cmdline := strings.ReplaceAll(string(data), "\x00", " ")
		for _, s := range substrs {
			if !strings.Contains(cmdline, s) {
				return false
			}
		}
		return true
	})
}

// ByArgv returns the PIDs, other than this process, whose argv (from
// /proc/<pid>/cmdline) satisfies match.
func ByArgv(match func(argv []string) bool) []int {
	self := os.Getpid()
	return scan(func(pidDir string) bool {
		pid, _ := strconv.Atoi(filepath.Base(pidDir))
		if pid == self {
			return false
		}
		data, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
		if err != nil || len(data) == 0 {
			return false
		}
		return match(strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"))
	})
}

// Signal sends sig to every pid and returns how many were signalled.
func Signal(pids []int, sig syscall.Signal) int {
	count := 0
	for _, pid := range pids {
		if err := syscall.Kill(pid, sig); err == nil {
			count++
		}
	}
	return count
}

func scan(match func(pidDir string) bool) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if match(filepath.Join("/proc", e.Name())) {
			pids = append(pids, pid)
		}
	}
	return pids
}
