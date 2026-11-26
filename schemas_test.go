package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRegisterSchemasSendsEveryEmbeddedSchema(t *testing.T) {
	f := newFake()
	f.handlers["korpus.register"] = func(json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil }
	k := NewKron(f, Config{Backoff: time.Millisecond})
	if err := k.registerSchemas(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir("schemas")
	if f.count("korpus.register") != len(files) || len(files) == 0 {
		t.Fatalf("registered %d of %d schemas", f.count("korpus.register"), len(files))
	}
	var got struct {
		Type   string         `json:"type"`
		Schema map[string]any `json:"schema"`
	}
	json.Unmarshal(f.calls["korpus.register"][0], &got)
	if got.Type == "" || got.Schema["type"] != "object" {
		t.Fatalf("bad register payload: %s", f.calls["korpus.register"][0])
	}
}

func TestRegisterWaitsForKorpus(t *testing.T) {
	f := newFake()
	n := 0
	f.handlers["korpus.register"] = func(json.RawMessage) (any, error) {
		if n++; n < 3 {
			return nil, errors.New("not up yet")
		}
		return map[string]any{"ok": true}, nil
	}
	k := NewKron(f, Config{Backoff: time.Millisecond})
	if err := k.register(context.Background(), "x", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("attempts = %d", n)
	}
	f.handlers["korpus.register"] = func(json.RawMessage) (any, error) { return nil, errors.New("down") }
	if err := k.register(context.Background(), "x", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("want last error, got %v", err)
	}
}
