package operations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryPageNamesBoundedSelection(t *testing.T) {
	dir := t.TempDir()
	for i := 599; i >= 0; i-- {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.eml", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var after string
	total := 0
	for {
		f, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		names, err := inventoryPageNames(context.Background(), f, after, 38)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		more := len(names) > 37
		if more {
			names = names[:37]
		}
		for _, name := range names {
			if want := fmt.Sprintf("%04d.eml", total); name != want {
				t.Fatalf("got %s want %s", name, want)
			}
			total++
			after = name
		}
		if !more {
			break
		}
	}
	if total != 600 {
		t.Fatal(total)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inventoryPageNames(ctx, f, "", 10); err == nil {
		t.Fatal("ignored cancellation")
	}
}
