//go:build !linux

package main

// canPrompt is false off Linux; dzman does not wait for Enter there.
func canPrompt() bool { return false }
