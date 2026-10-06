package main

import "syscall"

// diskUsage returns the bytes actually allocated to a (possibly sparse) file.
func diskUsage(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return st.Blocks * 512
}
