//go:build !windows

package main

func runNativeWindow(url string) bool { return false }

func openURL(u string) {}
