// Package luart runs a definition's Lua script to resolve a version and URL.
// Scripts get HTTP, JSON/YAML, jq, CSS selectors and the GitHub API; no
// filesystem or processes.
package luart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// Timeout bounds one script run, HTTP included.
var Timeout = 60 * time.Second

// Env is what a script can see and use.
type Env struct {
	Client *fetch.Client
	App    *def.App
	Vars   def.Vars
}

// Result is the table a script returns.
type Result struct {
	Version  string
	Display  string
	URL      string
	Filename string
	SHA256   string
	Headers  map[string]string
}

// SoftFailure is a script's `return nil, reason`: nothing to install right
// now.
type SoftFailure struct{ Reason string }

func (e *SoftFailure) Error() string { return e.Reason }

// Resolve runs code (chunk is its name in error messages).
func Resolve(ctx context.Context, env Env, chunk, code string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	L.SetContext(ctx)
	openSandbox(L)
	(&api{ctx: ctx, env: env}).install(L)

	fn, err := L.Load(strings.NewReader(code), chunk)
	if err != nil {
		return nil, fmt.Errorf("lua: %w", err)
	}
	if err := L.CallByParam(lua.P{Fn: fn, NRet: 2, Protect: true}); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("lua: script timed out after %s", Timeout)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("lua: %s", trimTraceback(err.Error()))
	}
	first, second := L.Get(-2), L.Get(-1)

	tbl, ok := first.(*lua.LTable)
	if !ok {
		if first == lua.LNil || first == lua.LFalse {
			reason := "script found nothing to install"
			if s, ok := second.(lua.LString); ok && s != "" {
				reason = string(s)
			}
			return nil, &SoftFailure{Reason: reason}
		}
		return nil, fmt.Errorf("lua: script must return a table {version=..., url=...} or nil, \"reason\"; got %s", first.Type())
	}
	res := &Result{
		Version:  field(tbl, "version"),
		Display:  field(tbl, "display"),
		URL:      field(tbl, "url"),
		Filename: field(tbl, "filename"),
		SHA256:   field(tbl, "sha256"),
	}
	if h, ok := tbl.RawGetString("headers").(*lua.LTable); ok {
		res.Headers = stringMap(h)
	}
	if res.Version == "" || res.URL == "" {
		return nil, errors.New("lua: the returned table needs non-empty version and url")
	}
	return res, nil
}

// field reads a string field; numbers are accepted (build numbers).
func field(t *lua.LTable, key string) string {
	switch v := t.RawGetString(key).(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return v.String()
	}
	return ""
}

func stringMap(t *lua.LTable) map[string]string {
	m := map[string]string{}
	t.ForEach(func(k, v lua.LValue) { m[k.String()] = v.String() })
	return m
}

// openSandbox loads the language libraries only: no io, no package/require,
// no debug, and from os just the clock.
func openSandbox(L *lua.LState) {
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
		{lua.OsLibName, lua.OpenOs},
	} {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	for _, name := range []string{"dofile", "loadfile", "require", "module", "newproxy"} {
		L.SetGlobal(name, lua.LNil)
	}
	full := L.GetGlobal(lua.OsLibName).(*lua.LTable)
	clock := L.NewTable()
	for _, name := range []string{"time", "date", "clock", "difftime"} {
		clock.RawSetString(name, full.RawGetString(name))
	}
	L.SetGlobal(lua.OsLibName, clock)
}

// trimTraceback keeps the message and drops gopher-lua's Go-side stack.
func trimTraceback(msg string) string {
	if i := strings.Index(msg, "\nstack traceback:"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}
