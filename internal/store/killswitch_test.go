package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestKillSwitch(t *testing.T) {
	dir := t.TempDir()
	if err := CheckHalt(dir); err != nil {
		t.Fatalf("sending blocked with no panic file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PanicFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckHalt(dir); !errors.Is(err, ErrHalted) {
		t.Errorf("panic file did not halt sending: %v", err)
	}
}

// An unreadable state directory is not evidence that sending is safe.
func TestKillSwitchFailsClosed(t *testing.T) {
	if err := CheckHalt("/proc/self/mem/not-a-directory"); !errors.Is(err, ErrHalted) {
		t.Errorf("an unreadable state directory permitted sending: %v", err)
	}
}
