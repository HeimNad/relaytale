package provider

import (
	"testing"
	"time"
)

func TestEvidenceRejectsUnboundedAndSecretReferences(t *testing.T) {
	now := time.Now()
	base := Evidence{Topic: "smtputf8", Value: "supported", Source: "operator_report", ObservedAt: now}
	if base.Validate(now) != nil {
		t.Fatal("valid claim rejected")
	}
	for _, reference := range []string{"https://user:password@example.test/docs", "https://example.test/?token=secret", "https://example.test/#secret", "http://example.test/docs"} {
		e := base
		e.SourceURL = reference
		if e.Validate(now) == nil {
			t.Fatal("unsafe reference accepted")
		}
	}
	for _, topic := range []string{"unknown", "password"} {
		e := base
		e.Topic = topic
		if e.Validate(now) == nil {
			t.Fatal("unbounded topic")
		}
	}
	e := base
	e.Source = "smtp_probe"
	if e.Validate(now) == nil {
		t.Fatal("operator promoted to probe evidence")
	}
	e = base
	e.ObservedAt = now.Add(time.Hour)
	if e.Validate(now) == nil {
		t.Fatal("future evidence")
	}
	e = base
	e.ObservedAt = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	if e.Validate(now) == nil {
		t.Fatal("unreadable database timestamp accepted")
	}
	e = base
	e.Topic = "max_message_bytes"
	e.Value = "001"
	if e.Validate(now) == nil {
		t.Fatal("ambiguous numeric claim")
	}
}
