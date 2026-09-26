package operations

import (
	"container/heap"
	"context"
	"errors"
	"io"
	"os"
	"sort"
)

// A max heap retains only the smallest page of names after the cursor.
// Every page enumerates the directory again, using O(page size + 256) memory.
type inventoryNames []string

func (h inventoryNames) Len() int           { return len(h) }
func (h inventoryNames) Less(i, j int) bool { return h[i] > h[j] }
func (h inventoryNames) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *inventoryNames) Push(v any)        { *h = append(*h, v.(string)) }
func (h *inventoryNames) Pop() any          { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }
func inventoryPageNames(ctx context.Context, folder *os.File, after string, count int) ([]string, error) {
	names := &inventoryNames{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := folder.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if name <= after {
				continue
			}
			if names.Len() < count {
				heap.Push(names, name)
			} else if name < (*names)[0] {
				(*names)[0] = name
				heap.Fix(names, 0)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	sort.Strings(*names)
	return []string(*names), nil
}
