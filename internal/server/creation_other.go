//go:build !linux

package server

func creationTime(path string) (int64, bool) { return 0, false }
