package api

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestSendKeysRequireIndependentShapeAndPolicy(t *testing.T) {
	key := SendKey{ID: uuid.NewString(), Token: "rts_" + strings.Repeat("a", 64), AllowedFrom: []string{"Sender@EXAMPLE.test"}}
	raw, _ := json.Marshal([]SendKey{key})
	keys, e := ParseSendKeys(string(raw))
	if e != nil || keys[0].AllowedFrom[0] != "Sender@example.test" {
		t.Fatal(keys, e)
	}
	for _, change := range []func(*SendKey){func(k *SendKey) { k.ID = "reader" }, func(k *SendKey) { k.Token = strings.Repeat("a", 68) }, func(k *SendKey) { k.AllowedFrom = nil }, func(k *SendKey) { k.AllowedFrom = []string{"Name <sender@example.test>"} }} {
		bad := key
		change(&bad)
		b, _ := json.Marshal([]SendKey{bad})
		if _, e = ParseSendKeys(string(b)); e == nil {
			t.Fatal("invalid key accepted")
		}
	}
	duplicate, _ := json.Marshal([]SendKey{key, key})
	if _, e = ParseSendKeys(string(duplicate)); e == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, e = ParseSendKeys(`[{"id":"one","id":"two"}]`); e == nil {
		t.Fatal("duplicate fields accepted")
	}
}

func TestSendBudgetConfigurationBounds(t *testing.T) {
	for _, budget := range []string{`{"minute":-1}`, `{"hour":2147483648}`, `{"day":-1}`, `{"hour":1.5}`, `{"minute":1,"minute":2}`, `{"burst":1}`} {
		raw := `[{"id":"` + uuid.NewString() + `","token":"rts_` + strings.Repeat("a", 64) + `","allowed_from":["sender@example.test"],"budget":` + budget + `}]`
		if _, err := ParseSendKeys(raw); err == nil {
			t.Fatal("invalid budget accepted", budget)
		}
	}
}

func TestSendEnvironmentIsExplicitAndValidated(t *testing.T) {
	for _, env := range []string{"", "production", "sandbox", "test", "Sandbox"} {
		key := SendKey{ID: uuid.NewString(), Token: "rts_" + strings.Repeat("a", 64), AllowedFrom: []string{"sender@example.test"}, Environment: env}
		raw, _ := json.Marshal([]SendKey{key})
		keys, err := ParseSendKeys(string(raw))
		if env == "test" || env == "Sandbox" {
			if err == nil {
				t.Fatal("invalid environment accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		expected := env
		if env == "" {
			expected = "production"
		}
		if keys[0].Environment != expected {
			t.Fatal(keys)
		}
	}
}
