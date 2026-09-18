package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrHalted is returned when the kill switch is engaged.
var ErrHalted = errors.New("store: sending is halted")

// PanicFile is the kill switch. Its presence stops sending immediately, with
// no restart and no configuration change needed:
//
//	touch /var/lib/wa-bridge/PANIC
//
// Checked before every bubble rather than once per candidate, so engaging it
// stops a multi-bubble message partway rather than after it finishes.
const PanicFile = "PANIC"

// CheckHalt reports whether sending is permitted.
//
// It fails closed: if the state directory cannot be read at all, sending stops.
// An unreadable disk is not evidence that sending is safe (SEC-5).
func CheckHalt(stateDir string) error {
	path := filepath.Join(stateDir, PanicFile)
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s exists", ErrHalted, path)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("%w: cannot read %s: %v", ErrHalted, path, err)
	}
}
