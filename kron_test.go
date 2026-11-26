package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knoniewicz/kor/messages"
)

type fakeKor struct {
	mu       sync.Mutex
	handlers map[string]func(json.RawMessage) (any, error)
	calls    map[string][]json.RawMessage
	pubs     []string
}

func newFake() *fakeKor {
	return &fakeKor{handlers: map[string]func(json.RawMessage) (any, error){}, calls: map[string][]json.RawMessage{}}
}

func (f *fakeKor) Call(ctx context.Context, subject string, payload any) (*messages.Message, error) {
	b, _ := json.Marshal(payload)
	f.mu.Lock()
	f.calls[subject] = append(f.calls[subject], b)
	h := f.handlers[subject]
	f.mu.Unlock()
	if h == nil {
		return nil, messages.ErrorPayload{Code: messages.ErrorCodeHandlerNotFound, Message: subject}
	}
	out, err := h(b)
	if err != nil {
		return nil, err
	}
	rb, _ := json.Marshal(out)
	return &messages.Message{Payload: rb}, nil
}

func (f *fakeKor) Publish(subject string, _ any, _ messages.PublicationDescriptor) error {
	f.mu.Lock()
	f.pubs = append(f.pubs, subject)
	f.mu.Unlock()
	return nil
}

func (f *fakeKor) count(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[s])
}

func okWrite(f *fakeKor) {
	f.handlers["korpus.write"] = func(b json.RawMessage) (any, error) {
		return map[string]any{"ok": true, "id": "x", "updated_at": "now"}, nil
	}
}

func jobFrom(t *testing.T, src string) *Job {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "automations"), 0o755)
	os.WriteFile(filepath.Join(dir, "automations", "t.lua"), []byte(src), 0o644)
	jobs, err := loadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return jobs["t"]
}

func TestSandboxDeniesOSAndIO(t *testing.T) {
	f := newFake()
	okWrite(f)
	k := NewKron(f, Config{Backoff: time.Millisecond})
	for _, expr := range []string{"os.execute('true')", "io.open('/etc/passwd')", "dofile('x')", "require('os')", "debug.traceback()", "loadfile('x')"} {
		j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx) `+expr+` end }`)
		if rec := k.Run(context.Background(), j, nil); rec.OK {
			t.Errorf("%s was allowed", expr)
		}
	}
}

func TestWriteQueryAndRunRecord(t *testing.T) {
	f := newFake()
	okWrite(f)
	f.handlers["korpus.query"] = func(b json.RawMessage) (any, error) {
		return map[string]any{"records": []any{map[string]any{"id": "a"}}, "count": 1}, nil
	}
	k := NewKron(f, Config{Backoff: time.Millisecond})
	j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx)
	  ctx.write("brief", "id1", {text="hi"}, {source="s"})
	  local r = ctx.query("brief", {where={a=1}, limit=5})
	  if r.count ~= 1 or r.records[1].id ~= "a" then error("bad query result") end
	end }`)
	rec := k.Run(context.Background(), j, nil)
	if !rec.OK {
		t.Fatalf("run failed: %s", rec.Error)
	}

	if n := f.count("korpus.write"); n != 2 {
		t.Fatalf("writes = %d", n)
	}
	var w struct {
		Type, ID string
		Data     RunRecord
	}
	json.Unmarshal(f.calls["korpus.write"][1], &w)
	if w.Type != "automation.run" || w.Data.Name != "t" || w.Data.Kind != "automation" || !w.Data.OK {
		t.Fatalf("bad run record %s", f.calls["korpus.write"][1])
	}
	var q map[string]any
	json.Unmarshal(f.calls["korpus.query"][0], &q)
	if q["type"] != "brief" || q["limit"] != float64(5) {
		t.Fatalf("bad query payload %v", q)
	}
}

func TestFailedRunIsRecordedAndKorpusDownIsSurvived(t *testing.T) {
	f := newFake()
	k := NewKron(f, Config{Backoff: time.Millisecond})
	j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx) error("boom") end }`)
	rec := k.Run(context.Background(), j, nil)
	if rec.OK || !strings.Contains(rec.Error, "boom") {
		t.Fatalf("rec = %+v", rec)
	}
	if n := f.count("korpus.write"); n != 4 {
		t.Fatalf("expected 4 attempts, got %d", n)
	}
}

func TestWriteDoesNotRetryTypedErrors(t *testing.T) {
	f := newFake()
	f.handlers["korpus.write"] = func(json.RawMessage) (any, error) {
		return nil, messages.ErrorPayload{Code: messages.ErrorCodeHandlerFailed, Message: "invalid: nope"}
	}
	k := NewKron(f, Config{Backoff: time.Millisecond})
	j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx) ctx.write("x","1",{}) end }`)
	rec := k.Run(context.Background(), j, nil)
	if rec.OK || !strings.Contains(rec.Error, messages.ErrorCodeHandlerFailed) {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestAskFallback(t *testing.T) {
	f := newFake()
	okWrite(f)
	k := NewKron(f, Config{Backoff: time.Millisecond})

	// agent down: ctx.ask hands back the raw data and ok=false so automations can still deliver
	j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx)
	  local r, ok = ctx.ask("sum", {a=1})
	  if ok or type(r) ~= "table" or r.a ~= 1 then error("no fallback") end
	end }`)
	if rec := k.Run(context.Background(), j, nil); !rec.OK {
		t.Fatalf("fallback: %s", rec.Error)
	}

	f.handlers["agent.ask"] = func(b json.RawMessage) (any, error) { return map[string]string{"text": "short"}, nil }
	j = jobFrom(t, `automation "t" { every = "1h", run = function(ctx)
	  local r, ok = ctx.ask("sum", {a=1})
	  if not ok or r ~= "short" then error("bad") end end }`)
	if rec := k.Run(context.Background(), j, nil); !rec.OK {
		t.Fatalf("ask: %s", rec.Error)
	}
	var p struct{ Prompt string }
	json.Unmarshal(f.calls["agent.ask"][0], &p)
	if !strings.Contains(p.Prompt, `{"a":1}`) {
		t.Fatalf("prompt lacks data: %q", p.Prompt)
	}
}

func TestCallRaisesTypedError(t *testing.T) {
	f := newFake()
	okWrite(f)
	k := NewKron(f, Config{Backoff: time.Millisecond})
	j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx)
	  local ok, err = pcall(ctx.call, "nope.x", {})
	  if ok or not string.find(err, "`+messages.ErrorCodeHandlerNotFound+`") then error("got " .. tostring(err)) end
	end }`)
	if rec := k.Run(context.Background(), j, nil); !rec.OK {
		t.Fatal(rec.Error)
	}
}

func TestEnvAllowListAndTimeout(t *testing.T) {
	t.Setenv("KRON_T", "v")
	f := newFake()
	okWrite(f)
	k := NewKron(f, Config{EnvAllow: []string{"KRON_T"}, Timeout: 200 * time.Millisecond, Backoff: time.Millisecond})
	j := jobFrom(t, `automation "t" { every = "1h", run = function(ctx)
	  assert(ctx.env("KRON_T") == "v")
	  assert(not pcall(ctx.env, "HOME"))
	  while true do end
	end }`)
	rec := k.Run(context.Background(), j, nil)
	if rec.OK || !strings.Contains(rec.Error, "timeout") {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestParseTrigger(t *testing.T) {
	for in, want := range map[[3]string]string{
		{"30m", "", ""}:    "@every 30m",
		{"", "06:00", ""}:  "0 6 * * *",
		{"", "18:30", ""}:  "30 18 * * *",
		{"", "", "jobs.*"}: "",
	} {
		got, err := parseTrigger(in[0], in[1], in[2])
		if err != nil || got != want {
			t.Errorf("%v -> %q, %v", in, got, err)
		}
	}
	for _, in := range [][3]string{{"", "", ""}, {"1h", "06:00", ""}, {"", "25:00", ""}, {"", "6:00", ""}, {"soon", "", ""}} {
		if _, err := parseTrigger(in[0], in[1], in[2]); err == nil {
			t.Errorf("%v should fail", in)
		}
	}
}

func TestLastDue(t *testing.T) {
	loc := time.UTC
	if d := lastDue("06:00", time.Date(2026, 10, 13, 7, 0, 0, 0, loc)); d.Day() != 13 {
		t.Error(d)
	}
	if d := lastDue("06:00", time.Date(2026, 10, 13, 5, 0, 0, 0, loc)); d.Day() != 12 {
		t.Error(d)
	}
}

func TestExampleLuaLoads(t *testing.T) {
	jobs, err := loadDir("lua/examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"hello", "note-example"} {
		if jobs[n] == nil {
			t.Errorf("missing %s", n)
		}
	}
}

func TestEventJobGetsEvent(t *testing.T) {
	f := newFake()
	okWrite(f)
	k := NewKron(f, Config{Backoff: time.Millisecond})
	j := jobFrom(t, `automation "t" { on = "a.*", run = function(ctx, ev)
	  assert(ev.subject == "a.b" and ev.payload.n == 3) end }`)
	if rec := k.Run(context.Background(), j, map[string]any{"subject": "a.b", "payload": map[string]any{"n": 3}}); !rec.OK {
		t.Fatal(rec.Error)
	}
}
