package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	luajson "layeh.com/gopher-json"
)

type Job struct {
	Name, Kind, File string
	Every, At, On    string
	Catchup          bool

	fn *lua.LFunction
	L  *lua.LState
	mu *sync.Mutex
}

type JobInfo struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Every   string `json:"every,omitempty"`
	At      string `json:"at,omitempty"`
	On      string `json:"on,omitempty"`
	Catchup bool   `json:"catchup,omitempty"`
	File    string `json:"file"`
}

func (j *Job) Info() JobInfo {
	return JobInfo{j.Name, j.Kind, j.Every, j.At, j.On, j.Catchup, j.File}
}

func sortInfos(s []JobInfo) {
	sort.Slice(s, func(a, b int) bool { return s[a].Name < s[b].Name })
}

func (j *Job) Spec() (string, error) { return parseTrigger(j.Every, j.At, j.On) }

func parseTrigger(every, at, on string) (string, error) {
	n := 0
	for _, s := range []string{every, at, on} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return "", fmt.Errorf("exactly one of every, at, on is required")
	}
	switch {
	case every != "":
		d, err := time.ParseDuration(every)
		if err != nil || d < time.Second {
			return "", fmt.Errorf("bad every %q", every)
		}
		return "@every " + every, nil
	case at != "":
		var h, m int
		if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(at) != 5 {
			return "", fmt.Errorf("bad at %q (want HH:MM)", at)
		}
		return fmt.Sprintf("%d %d * * *", m, h), nil
	}
	return "", nil
}

func lastDue(at string, now time.Time) time.Time {
	var h, m int
	fmt.Sscanf(at, "%d:%d", &h, &m)
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

func loadDir(dir string) (map[string]*Job, error) {
	jobs := map[string]*Job{}
	for _, kind := range []string{"connector", "automation"} {
		files, _ := filepath.Glob(filepath.Join(dir, kind+"s", "*.lua"))
		sort.Strings(files)
		for _, f := range files {
			if err := loadFile(f, kind, jobs); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
		}
	}
	return jobs, nil
}

func loadFile(path, kind string, jobs map[string]*Job) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	L := newState()
	mu := &sync.Mutex{}
	var defErr error
	define := func(L *lua.LState) int {
		name := L.CheckString(1)
		t := L.CheckTable(2)
		str := func(k string) string { s, _ := t.RawGetString(k).(lua.LString); return string(s) }
		fn, ok := t.RawGetString("run").(*lua.LFunction)
		if !ok {
			defErr = fmt.Errorf("%s %q: run must be a function", kind, name)
			return 0
		}
		j := &Job{Name: name, Kind: kind, File: path, Every: str("every"), At: str("at"), On: str("on"),
			Catchup: t.RawGetString("catchup") == lua.LTrue, fn: fn, L: L, mu: mu}
		if _, err := parseTrigger(j.Every, j.At, j.On); err != nil {
			defErr = fmt.Errorf("%s %q: %w", kind, name, err)
			return 0
		}
		if kind == "connector" && j.On != "" {
			defErr = fmt.Errorf("connector %q: use automation for event triggers", name)
			return 0
		}
		if _, dup := jobs[name]; dup {
			defErr = fmt.Errorf("duplicate job name %q", name)
			return 0
		}
		jobs[name] = j
		return 0
	}

	L.SetGlobal(kind, L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		L.Push(L.NewFunction(func(L *lua.LState) int {
			L.Insert(lua.LString(name), 1)
			return define(L)
		}))
		return 1
	}))
	fn, err := L.Load(bytesReader(src), path)
	if err != nil {
		return err
	}
	L.Push(fn)
	if err := L.PCall(0, 0, nil); err != nil {
		return err
	}
	return defErr
}

func (j *Job) execute(parent context.Context, k *Kron, event map[string]any) (err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, k.cfg.Timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	j.L.SetContext(ctx)
	defer j.L.RemoveContext()
	rc := &runCtx{k: k, name: j.Name, ctx: ctx}
	args := []lua.LValue{rc.table(j.L)}
	if j.Kind == "automation" {
		ev := lua.LValue(lua.LNil)
		if event != nil {
			b, _ := jsonMarshal(event)
			ev, _ = luajson.Decode(j.L, b)
		}
		args = append(args, ev)
	}
	if err := j.L.CallByParam(lua.P{Fn: j.fn, NRet: 0, Protect: true}, args...); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("timeout after %s: %v", k.cfg.Timeout, err)
		}
		return err
	}
	return nil
}
