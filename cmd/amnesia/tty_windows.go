//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// GetConsoleMode succeeds only for a real console handle. It is the documented
// way to ask this on Windows, and the only reliable one.
//
// The portable-looking alternative does not work here. os.Stdin.Stat() on
// Windows returns a FileInfo with no file-index information for anything that
// is not a regular file, and os.Stat("NUL") returns the same emptiness - so
// os.SameFile(stdin, NUL) answers "true" for a console, a pipe and the null
// device alike. That check shipped, and it made amnesia refuse to run anything
// on Windows at all: `amnesia setup` said it needed a terminal while sitting in
// one, and every confirm prompt declined itself.
var procGetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleMode")

func isTerminal() bool {
	var mode uint32
	ret, _, _ := procGetConsoleMode.Call(os.Stdin.Fd(), uintptr(unsafe.Pointer(&mode)))
	return ret != 0
}
