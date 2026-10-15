//go:build darwin

// Package host reads facts about local processes and user input from the
// macOS kernel and window server. Nothing here needs Accessibility permission.
package host

/*
#cgo LDFLAGS: -framework CoreGraphics
#include <sys/types.h>
#include <sys/sysctl.h>
#include <libproc.h>
#include <CoreGraphics/CoreGraphics.h>

typedef struct {
	int ok;
	int ppid;
	int pgid;
	int tpgid;
	long long start_sec;
	int start_usec;
} qg_proc;

static qg_proc qg_lookup(int pid) {
	qg_proc out = {0};
	struct kinfo_proc info;
	size_t length = sizeof(info);
	int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, pid};
	if (pid <= 0 || sysctl(mib, 4, &info, &length, NULL, 0) != 0 || length == 0 || info.kp_proc.p_pid != pid) {
		return out;
	}
	out.ok = 1;
	out.ppid = info.kp_eproc.e_ppid;
	out.pgid = info.kp_eproc.e_pgid;
	out.tpgid = info.kp_eproc.e_tpgid;
	out.start_sec = info.kp_proc.p_starttime.tv_sec;
	out.start_usec = info.kp_proc.p_starttime.tv_usec;
	return out;
}

static int qg_path(int pid, char *buffer, int size) {
	if (pid <= 0) {
		return 0;
	}
	return proc_pidpath(pid, buffer, (uint32_t)size);
}

static double qg_idle_seconds(void) {
	return CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateHIDSystemState, kCGAnyInputEventType);
}
*/
import "C"

import (
	"math"
	"time"
)

// Process is a kernel snapshot of one process.
type Process struct {
	PID   int
	PPID  int
	PGID  int
	TPGID int // foreground process group of the controlling terminal; <=0 without one
	Start time.Time
}

// Foreground reports whether the process group owns its controlling terminal.
func (p Process) Foreground() bool { return p.TPGID > 0 && p.PGID == p.TPGID }

// Lookup reads one process from the kernel process table.
func Lookup(pid int) (Process, bool) {
	r := C.qg_lookup(C.int(pid))
	if r.ok == 0 {
		return Process{}, false
	}
	return Process{PID: pid, PPID: int(r.ppid), PGID: int(r.pgid), TPGID: int(r.tpgid),
		Start: time.Unix(int64(r.start_sec), int64(r.start_usec)*1000)}, true
}

// ParentPID returns the parent of pid, or -1 when it cannot be read.
func ParentPID(pid int) int {
	p, ok := Lookup(pid)
	if !ok {
		return -1
	}
	return p.PPID
}

// ExecutablePath is the kernel's record of the file pid is executing, or "".
func ExecutablePath(pid int) string {
	buffer := make([]C.char, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.qg_path(C.int(pid), &buffer[0], C.int(len(buffer)))
	if n <= 0 {
		return ""
	}
	return C.GoStringN(&buffer[0], n)
}

// UserIdle is the time since the last keyboard, mouse or trackpad event.
func UserIdle() (time.Duration, bool) {
	s := float64(C.qg_idle_seconds())
	if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 {
		return 0, false
	}
	return time.Duration(s * float64(time.Second)), true
}
