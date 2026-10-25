//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var oodle *syscall.LazyProc

func initOodle(game string) error {
	dll := syscall.NewLazyDLL(filepath.Join(game, "bin", "x64", "oo2ext_7_win64.dll"))
	if err := dll.Load(); err != nil {
		return err
	}
	oodle = dll.NewProc("OodleLZ_Decompress")
	return oodle.Find()
}

func oodleDecompress(comp []byte, rawLen int) ([]byte, error) {
	if oodle == nil {
		return nil, fmt.Errorf("Oodle nicht geladen")
	}
	out := make([]byte, rawLen+64)
	r, _, _ := oodle.Call(uintptr(unsafe.Pointer(&comp[0])), uintptr(len(comp)), uintptr(unsafe.Pointer(&out[0])), uintptr(rawLen),
		1, 1, 0, 0, 0, 0, 0, 0, 0, 3)
	if int(r) != rawLen {
		return nil, fmt.Errorf("Oodle: %d statt %d Bytes", int(r), rawLen)
	}
	return out[:rawLen], nil
}
