package suppression

import (
	"context"
	"strings"
	"testing"
)

func TestRejectInvalidOperatorInputBeforeDatabase(t *testing.T) {
	for _, email := range []string{"", "Name <one@example.test>", "one@example.test, two@example.test", "one@example.test\r\nX: bad", "one@example.test\x00", "not-a-mailbox"} {
		if _, err := (Service{}).Add(context.Background(), AddRequest{Email: email, Category: "manual", Actor: "op", Reason: "checked"}); err == nil {
			t.Fatalf("accepted %q", email)
		}
	}
	for _, v := range []AddRequest{
		{Email: "one@example.test", Category: "hard_bounce", Actor: "", Reason: "checked"},
		{Email: "one@example.test", Category: "manual", Actor: "op", Reason: " "},
		{Email: "one@example.test", Category: "manual", Actor: strings.Repeat("x", 129), Reason: "checked"},
		{Email: "one@example.test", Category: "automatic-untrusted-feedback", Actor: "op", Reason: "header claim"},
	} {
		if _, err := (Service{}).Add(context.Background(), v); err == nil {
			t.Fatal("invalid operator input accepted")
		}
	}
}
