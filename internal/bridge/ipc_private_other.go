//go:build !darwin && !linux && !freebsd

package bridge

import (
	"errors"
	"os"
)

func ownedByCurrentUser(os.FileInfo) bool { return false }
func openPrivateToken(string) (*os.File, error) {
	return nil, errors.New("private desktop IPC is unsupported on this platform")
}
