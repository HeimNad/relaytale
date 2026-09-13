package provider

import (
	"context"
	"errors"
	"testing"
)

func TestManagementRejectsReservedOrNonCanonicalIDs(t *testing.T) {
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "urn:uuid:11111111-1111-4111-8111-111111111111"} {
		_, err := Manage(context.Background(), nil, nil, "create", id, 0, Settings{}, "", "operator", "test")
		if !errors.Is(err, ErrInvalid) {
			t.Fatal("reserved pagination ID accepted", err)
		}
	}
}
