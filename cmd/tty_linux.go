package main

import (
	"syscall"
	"unsafe"
)

func isTTY(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}

// canPrompt reports whether reading a line from stdin is safe and useful:
// stdin and stderr are terminals and dzman is in the terminal's foreground
// process group. A background job that reads its terminal gets stopped.
func canPrompt() bool {
	if !isTTY(0) || !isTTY(2) {
		return false
	}
	var pgrp int32
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgrp)))
	return e == 0 && int(pgrp) == syscall.Getpgrp()
}
