package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knoniewicz/kor/messages"
)

const testJobSrc = `automation "t" { every = "1h", run = function(ctx) end }`

func sourceKron(t *testing.T, files map[string]string) (*Kron, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "automations"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, "automations", name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := loadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	k := NewKron(newFake(), Config{Dir: dir})
	k.jobs = jobs
	return k, dir
}

func TestSourceReturnsLoadedJob(t *testing.T) {
	k, dir := sourceKron(t, map[string]string{"t.lua": testJobSrc})
	wantFile := filepath.Join(dir, "automations", "t.lua")

	out, err := k.readSource("t")
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "t" || out.File != wantFile || out.Lua != testJobSrc {
		t.Fatalf("out = %+v, want file %s", out, wantFile)
	}

	// The handler's request shape is {name}; its response is {name, file, lua}.
	res, err := k.source(context.Background(), messages.Message{Payload: json.RawMessage(`{"name":"t"}`)})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := res.(sourceOut)
	if !ok || got != out {
		t.Fatalf("handler result = %#v", res)
	}
	b, _ := json.Marshal(got)
	var raw map[string]any
	json.Unmarshal(b, &raw)
	for _, field := range []string{"name", "file", "lua"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("response missing %q: %s", field, b)
		}
	}
	if len(raw) != 3 {
		t.Errorf("response has extra fields: %s", b)
	}
}

func TestSourceResolvesOnlyByName(t *testing.T) {
	k, _ := sourceKron(t, map[string]string{"t.lua": testJobSrc})
	for _, name := range []string{"", "nope", "../automations/t", "../../etc/passwd", "/etc/passwd", "automations/t.lua", "t.lua"} {
		if _, err := k.readSource(name); messages.CodeOf(err) != messages.ErrorCodeNotFound {
			t.Errorf("%q: err = %v, want not_found", name, err)
		}
	}
}

func TestSourceRefusesFilesOutsideJobDir(t *testing.T) {
	k, dir := sourceKron(t, map[string]string{"t.lua": testJobSrc})
	outside := filepath.Join(t.TempDir(), "evil.lua")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	k.jobs["evil"] = &Job{Name: "evil", File: outside}
	if _, err := k.readSource("evil"); messages.CodeOf(err) != messages.ErrorCodeInvalid {
		t.Fatalf("err = %v, want invalid", err)
	}

	notLua := filepath.Join(dir, "automations", "t.txt")
	if err := os.WriteFile(notLua, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	k.jobs["txt"] = &Job{Name: "txt", File: notLua}
	if _, err := k.readSource("txt"); messages.CodeOf(err) != messages.ErrorCodeInvalid {
		t.Fatalf("err = %v, want invalid", err)
	}
}

func TestSourceCapsSize(t *testing.T) {
	k, dir := sourceKron(t, map[string]string{})
	big := filepath.Join(dir, "automations", "big.lua")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", maxSourceBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	k.jobs["big"] = &Job{Name: "big", File: big}
	if _, err := k.readSource("big"); messages.CodeOf(err) != messages.ErrorCodeInvalid || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want invalid size error", err)
	}

	ok := filepath.Join(dir, "automations", "ok.lua")
	if err := os.WriteFile(ok, []byte(strings.Repeat(" ", maxSourceBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	k.jobs["ok"] = &Job{Name: "ok", File: ok}
	if out, err := k.readSource("ok"); err != nil || len(out.Lua) != maxSourceBytes {
		t.Fatalf("at the cap: %d bytes, err %v", len(out.Lua), err)
	}
}

func TestSourceDescriptor(t *testing.T) {
	d, err := messages.HandlerDescriptorFor[nameIn, sourceOut]("Read a loaded job's own Lua source by name (read-only, max 256 KiB)")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d.InputSchema), `"name"`) {
		t.Errorf("input schema lacks name: %s", d.InputSchema)
	}
	for _, field := range []string{"name", "file", "lua"} {
		if !strings.Contains(string(d.OutputSchema), `"`+field+`"`) {
			t.Errorf("output schema lacks %q: %s", field, d.OutputSchema)
		}
	}
}
