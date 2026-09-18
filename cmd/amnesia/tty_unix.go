//go:build !windows

package main

import "os"

// isTerminal reports whether there is a human who can answer a prompt.
//
// The obvious check - is stdin a character device - is wrong, and wrong in the
// dangerous direction: /dev/null IS a character device. A macOS stress run
// caught amnesia executing a command under `< /dev/null`, because the prompt
// read EOF, took the empty answer as the [Y/n] default, and ran it. Every cron
// job, CI step, systemd unit and Makefile rule invokes programs exactly that way.
//
// stdlib has no tty check and x/term is a dependency this binary does not have.
// Excluding the null device covers the case that actually bites, and the EOF
// guard in askLine covers the rest. Unlike on Windows, os.SameFile is meaningful
// for device files here.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}
