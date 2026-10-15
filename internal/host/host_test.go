package host

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func TestLookupSelf(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kernel process table lookup is macOS-only")
	}
	p, ok := Lookup(os.Getpid())
	if !ok || p.PPID != os.Getppid() || p.PGID <= 0 || p.Start.IsZero() || p.Start.After(time.Now()) {
		t.Fatalf("unexpected self snapshot %+v ok=%v", p, ok)
	}
	if ParentPID(os.Getpid()) != os.Getppid() {
		t.Fatal("ParentPID disagrees with os.Getppid")
	}
	if _, ok := Lookup(-5); ok {
		t.Fatal("negative pid resolved")
	}
	if idle, ok := UserIdle(); ok && idle < 0 {
		t.Fatalf("negative idle %v", idle)
	}
}
