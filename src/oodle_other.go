//go:build !windows

package main

import "fmt"

func initOodle(string) error { return fmt.Errorf("nur Windows") }
func oodleDecompress([]byte, int) ([]byte, error) {
	return nil, fmt.Errorf("nur Windows")
}
