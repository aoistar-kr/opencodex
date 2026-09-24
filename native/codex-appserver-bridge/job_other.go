//go:build !windows

package main

// attachKillOnCloseJob is a no-op away from Windows. The bridge is Windows-first;
// on POSIX the child is reaped by the ordinary exit path instead.
func attachKillOnCloseJob(pid int) error {
	return nil
}
