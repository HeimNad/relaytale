package operations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapacityFileConfinementAndTypes(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(directory, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{{outside, "OUTSIDE_ROOT"}, {filepath.Join(directory, "link"), "SYMLINK_REVIEW"}, {filepath.Join(directory, "missing"), "MISSING"}, {filepath.Join(directory, "folder"), "NON_REGULAR_REVIEW"}} {
		got, size := capacityFile(root, directory, tc.path, 6)
		if got != tc.want || size != nil {
			t.Fatal(got, size, tc)
		}
	}
}
