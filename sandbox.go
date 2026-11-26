package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
	luajson "layeh.com/gopher-json"
)

func newState() *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	for _, n := range []string{"dofile", "loadfile", "load", "loadstring", "_printregs", "print"} {
		L.SetGlobal(n, lua.LNil)
	}
	return L
}

type runCtx struct {
	k    *Kron
	name string
	ctx  context.Context
}

func (rc *runCtx) table(L *lua.LState) *lua.LTable {
	t := L.NewTable()
	set := func(name string, fn lua.LGFunction) { t.RawSetString(name, L.NewFunction(fn)) }
	t.RawSetString("name", lua.LString(rc.name))

	set("log", func(L *lua.LState) int {
		var parts []string
		for i := 1; i <= L.GetTop(); i++ {
			parts = append(parts, L.ToStringMeta(L.Get(i)).String())
		}
		slog.Info(strings.Join(parts, " "), "job", rc.name)
		return 0
	})
	set("http_get", func(L *lua.LState) int { return rc.http(L, "GET", L.CheckString(1), nil, L.OptTable(2, nil)) })
	set("http_post", func(L *lua.LState) int {
		return rc.http(L, "POST", L.CheckString(1), L.Get(2), L.OptTable(3, nil))
	})
	set("json_decode", func(L *lua.LState) int {
		v, err := luajson.Decode(L, []byte(L.CheckString(1)))
		if err != nil {
			L.RaiseError("json_decode: %v", err)
		}
		L.Push(v)
		return 1
	})
	set("json_encode", func(L *lua.LState) int {
		b, err := luajson.Encode(L.CheckAny(1))
		if err != nil {
			L.RaiseError("json_encode: %v", err)
		}
		L.Push(lua.LString(b))
		return 1
	})
	set("sha1", func(L *lua.LState) int {
		s := sha1.Sum([]byte(L.CheckString(1)))
		L.Push(lua.LString(hex.EncodeToString(s[:])))
		return 1
	})
	set("sha256", func(L *lua.LState) int {
		s := sha256.Sum256([]byte(L.CheckString(1)))
		L.Push(lua.LString(hex.EncodeToString(s[:])))
		return 1
	})
	set("now", func(L *lua.LState) int { L.Push(lua.LString(time.Now().Format(time.RFC3339))); return 1 })
	set("today", func(L *lua.LState) int { L.Push(lua.LString(time.Now().Format("2006-01-02"))); return 1 })
	set("env", func(L *lua.LState) int {
		name := L.CheckString(1)
		v, ok := rc.k.getenv(name)
		if !ok {
			L.RaiseError("env %s is not allow-listed", name)
		}
		L.Push(lua.LString(v))
		return 1
	})
	set("call", func(L *lua.LState) int {
		payload := toGo(L.Get(2))
		res, err := rc.k.call(rc.ctx, L.CheckString(1), payload)
		if err != nil {
			L.RaiseError("%v", err)
		}
		return pushJSON(L, res)
	})
	set("publish", func(L *lua.LState) int {
		if err := rc.k.publish(L.CheckString(1), toGo(L.Get(2))); err != nil {
			L.RaiseError("publish: %v", err)
		}
		return 0
	})
	set("write", func(L *lua.LState) int {
		opts := L.OptTable(4, L.NewTable())
		rec := map[string]any{"type": L.CheckString(1), "id": L.CheckString(2), "data": toGo(L.CheckAny(3))}
		if s, ok := opts.RawGetString("source").(lua.LString); ok {
			rec["source"] = string(s)
		}
		res, err := rc.k.write(rc.ctx, rec)
		if err != nil {
			L.RaiseError("write: %v", err)
		}
		return pushJSON(L, res)
	})
	set("query", func(L *lua.LState) int {
		q := map[string]any{"type": L.CheckString(1)}
		if f, ok := toGo(L.OptTable(2, L.NewTable())).(map[string]any); ok {
			for k, v := range f {
				q[k] = v
			}
		}
		res, err := rc.k.call(rc.ctx, "korpus.query", q)
		if err != nil {
			L.RaiseError("query: %v", err)
		}
		return pushJSON(L, res)
	})
	set("ask", func(L *lua.LState) int {
		prompt, data := L.CheckString(1), L.Get(2)
		timeout := 90 * time.Second
		if o := L.OptTable(3, nil); o != nil {
			timeout = parseDur(o.RawGetString("timeout"), timeout)
		}
		text, err := rc.k.ask(rc.ctx, prompt, toGo(data), timeout)
		if err != nil {
			slog.Warn("ask fell back to raw data", "job", rc.name, "err", err)
			L.Push(data)
			L.Push(lua.LFalse)
			return 2
		}
		L.Push(lua.LString(text))
		L.Push(lua.LTrue)
		return 2
	})
	return t
}

func (rc *runCtx) http(L *lua.LState, method, url string, body lua.LValue, opts *lua.LTable) int {
	timeout := 30 * time.Second
	headers := map[string]string{}
	if opts != nil {
		timeout = parseDur(opts.RawGetString("timeout"), timeout)
		if h, ok := opts.RawGetString("headers").(*lua.LTable); ok {
			h.ForEach(func(k, v lua.LValue) { headers[k.String()] = v.String() })
		}
	}
	var rd io.Reader
	if body != nil && body != lua.LNil {
		if t, ok := body.(*lua.LTable); ok {
			b, err := json.Marshal(toGo(t))
			if err != nil {
				L.RaiseError("http: %v", err)
			}
			rd = strings.NewReader(string(b))
			if _, set := headers["Content-Type"]; !set {
				headers["Content-Type"] = "application/json"
			}
		} else {
			rd = strings.NewReader(body.String())
		}
	}
	ctx, cancel := context.WithTimeout(rc.ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		L.RaiseError("http: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		L.RaiseError("http: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		L.RaiseError("http: %v", err)
	}
	out, hs := L.NewTable(), L.NewTable()
	for k := range resp.Header {
		hs.RawSetString(strings.ToLower(k), lua.LString(resp.Header.Get(k)))
	}
	out.RawSetString("status", lua.LNumber(resp.StatusCode))
	out.RawSetString("body", lua.LString(b))
	out.RawSetString("headers", hs)
	L.Push(out)
	return 1
}

func parseDur(v lua.LValue, def time.Duration) time.Duration {
	switch x := v.(type) {
	case lua.LNumber:
		return time.Duration(float64(x) * float64(time.Second))
	case lua.LString:
		if d, err := time.ParseDuration(string(x)); err == nil {
			return d
		}
	}
	return def
}

func toGo(v lua.LValue) any {
	if v == nil || v == lua.LNil {
		return nil
	}
	b, err := luajson.Encode(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

func pushJSON(L *lua.LState, raw json.RawMessage) int {
	if len(raw) == 0 {
		L.Push(lua.LNil)
		return 1
	}
	v, err := luajson.Decode(L, raw)
	if err != nil {
		L.RaiseError("bad result: %v", err)
	}
	L.Push(v)
	return 1
}
