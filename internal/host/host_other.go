//go:build !darwin

// Package host reads facts about local processes and user input. Only macOS
// is implemented; other platforms report every fact as unavailable.
package host

import "time"

type Process struct {
	PID   int
	PPID  int
	PGID  int
	TPGID int
	Start time.Time
}

func (p Process) Foreground() bool { return p.TPGID > 0 && p.PGID == p.TPGID }

func Lookup(int) (Process, bool) { return Process{}, false }

func ParentPID(int) int { return -1 }

func ExecutablePath(int) string { return "" }

func UserIdle() (time.Duration, bool) { return 0, false }
