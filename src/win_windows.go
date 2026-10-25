//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// runNativeWindow: this build opens the interface in the default web browser (see openURL).
func runNativeWindow(url string) bool { return false }

// openURL opens a web address in the default browser (Windows shell API).
func openURL(u string) {
	sh := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
	verb, _ := syscall.UTF16PtrFromString("open")
	f, _ := syscall.UTF16PtrFromString(u)
	sh.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(f)), 0, 0, 1)
}
