package storage

import (
	"fmt"
	"os"
)

// CheckWritable verifies an actual write and fsync; directory permissions alone
// do not establish that the archive volume is usable.
func CheckWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".readiness-*")
	if err != nil {
		return fmt.Errorf("create storage probe: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write([]byte("ready")); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
