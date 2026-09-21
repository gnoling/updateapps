package luart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/itchyny/gojq"
	lua "github.com/yuin/gopher-lua"
	"gopkg.in/yaml.v3"

	"github.com/gnoling/updateapps/internal/fetch"
)

const maxBody = 32 << 20

// api is the Go side of everything scripts can call. See docs/LUA.md.
type api struct {
	ctx context.Context
	env Env
}

func (a *api) install(L *lua.LState) {
	module := func(name string, fns map[string]lua.LGFunction) {
		t := L.NewTable()
		L.SetFuncs(t, fns)
		L.SetGlobal(name, t)
	}
	module("http", map[string]lua.LGFunction{"get": a.httpGet, "head": a.httpHead, "post": a.httpPost, "post_json": a.httpPostJSON})
	module("json", map[string]lua.LGFunction{"decode": a.jsonDecode, "encode": a.jsonEncode})
	module("yaml", map[string]lua.LGFunction{"decode": a.yamlDecode})
	module("html", map[string]lua.LGFunction{"select": a.htmlSelect})
	module("gh", map[string]lua.LGFunction{"api": a.ghAPI})
	module("url", map[string]lua.LGFunction{"resolve": a.urlResolve, "basename": a.urlBasename})
	module("log", map[string]lua.LGFunction{"info": a.logAt(fetch.LogDetail), "warn": a.logAt(fetch.LogAlways)})
	L.SetGlobal("jq", L.NewFunction(a.jq(false)))
	L.SetGlobal("jq_all", L.NewFunction(a.jq(true)))
	L.SetGlobal("print", L.NewFunction(a.logAt(fetch.LogDetail)))

	cfg := L.NewTable()
	cfg.RawSetString("appdir", lua.LString(a.env.Vars.AppDir))
	cfg.RawSetString("appimagedir", lua.LString(a.env.Vars.AppImageDir))
	cfg.RawSetString("home", lua.LString(a.env.Vars.Home))
	if a.env.App != nil {
		cfg.RawSetString("name", lua.LString(a.env.App.ID))
		cfg.RawSetString("dest", lua.LString(a.env.App.Install.Dest))
	}
	L.SetGlobal("config", cfg)
}

// request makes an HTTP call for a script. Non-2xx is a result; a transport
// failure is a Lua error.
func (a *api) request(L *lua.LState, method, rawURL string, headers map[string]string, body []byte) (string, int, http.Header) {
	client := a.env.Client
	if a.env.App != nil && a.env.App.Insecure {
		client = client.Insecure()
	}
	resp, err := client.DoBody(a.ctx, method, rawURL, headers, body)
	var se *fetch.StatusError
	if errors.As(err, &se) {
		return se.Body, se.Code, http.Header{}
	}
	if err != nil {
		L.RaiseError("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		L.RaiseError("%s %s: %v", method, rawURL, err)
	}
	return string(data), resp.StatusCode, resp.Header
}

func optHeaders(L *lua.LState, n int) map[string]string {
	if t := L.OptTable(n, nil); t != nil {
		return stringMap(t)
	}
	return nil
}

// headerTable uses lower-case names so scripts needn't guess the casing.
func headerTable(L *lua.LState, h http.Header) *lua.LTable {
	t := L.NewTable()
	for k, v := range h {
		t.RawSetString(strings.ToLower(k), lua.LString(strings.Join(v, ", ")))
	}
	return t
}

// http.get(url, [headers]) -> body, status, headers
func (a *api) httpGet(L *lua.LState) int {
	body, status, h := a.request(L, http.MethodGet, L.CheckString(1), optHeaders(L, 2), nil)
	L.Push(lua.LString(body))
	L.Push(lua.LNumber(status))
	L.Push(headerTable(L, h))
	return 3
}

// http.head(url, [headers]) -> status, headers
func (a *api) httpHead(L *lua.LState) int {
	_, status, h := a.request(L, http.MethodHead, L.CheckString(1), optHeaders(L, 2), nil)
	L.Push(lua.LNumber(status))
	L.Push(headerTable(L, h))
	return 2
}

// http.post(url, body, [headers]) -> body, status, headers
func (a *api) httpPost(L *lua.LState) int {
	body, status, h := a.request(L, http.MethodPost, L.CheckString(1), optHeaders(L, 3), []byte(L.CheckString(2)))
	L.Push(lua.LString(body))
	L.Push(lua.LNumber(status))
	L.Push(headerTable(L, h))
	return 3
}

// http.post_json(url, table, [headers]) -> decoded response, status
func (a *api) httpPostJSON(L *lua.LState) int {
	payload, err := json.Marshal(fromLua(L.CheckAny(2)))
	if err != nil {
		L.RaiseError("http.post_json: %v", err)
	}
	headers := optHeaders(L, 3)
	if headers == nil {
		headers = map[string]string{}
	}
	if _, set := headers["Content-Type"]; !set {
		headers["Content-Type"] = "application/json"
	}
	body, status, _ := a.request(L, http.MethodPost, L.CheckString(1), headers, payload)
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		L.RaiseError("http.post_json: response (HTTP %d) isn't JSON: %v", status, err)
	}
	L.Push(toLua(L, v))
	L.Push(lua.LNumber(status))
	return 2
}

func (a *api) jsonDecode(L *lua.LState) int {
	var v any
	if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
		L.RaiseError("json.decode: %v", err)
	}
	L.Push(toLua(L, v))
	return 1
}

func (a *api) jsonEncode(L *lua.LState) int {
	out, err := json.Marshal(fromLua(L.CheckAny(1)))
	if err != nil {
		L.RaiseError("json.encode: %v", err)
	}
	L.Push(lua.LString(out))
	return 1
}

func (a *api) yamlDecode(L *lua.LState) int {
	var v any
	if err := yaml.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
		L.RaiseError("yaml.decode: %v", err)
	}
	L.Push(toLua(L, v))
	return 1
}

// jq(value, expr) -> first result; jq_all(value, expr) -> list of results.
// value is a table, or a string holding JSON.
func (a *api) jq(all bool) lua.LGFunction {
	return func(L *lua.LState) int {
		input := fromLua(L.CheckAny(1))
		if s, ok := input.(string); ok {
			var decoded any
			if json.Unmarshal([]byte(s), &decoded) == nil {
				input = decoded
			}
		}
		query, err := gojq.Parse(L.CheckString(2))
		if err != nil {
			L.RaiseError("jq: %v", err)
		}
		results := L.NewTable()
		iter := query.RunWithContext(a.ctx, input)
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err, isErr := v.(error); isErr {
				L.RaiseError("jq: %v", err)
			}
			if !all {
				L.Push(toLua(L, v))
				return 1
			}
			results.Append(toLua(L, v))
		}
		if !all {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(results)
		return 1
	}
}

// html.select(body, selector, [attr]) -> each match's text, or attr. Works on
// XML too; tag names match case-insensitively.
func (a *api) htmlSelect(L *lua.LState) int {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(L.CheckString(1))))
	if err != nil {
		L.RaiseError("html.select: %v", err)
	}
	// goquery quietly matches nothing for a selector it can't parse.
	sel, err := cascadia.Compile(L.CheckString(2))
	if err != nil {
		L.RaiseError("html.select: bad selector %q: %v", L.CheckString(2), err)
	}
	attr := L.OptString(3, "")
	out := L.NewTable()
	doc.FindMatcher(sel).Each(func(_ int, s *goquery.Selection) {
		v := strings.TrimSpace(s.Text())
		if attr != "" {
			v = strings.TrimSpace(s.AttrOr(attr, ""))
		}
		if v != "" {
			out.Append(lua.LString(v))
		}
	})
	L.Push(out)
	return 1
}

// gh.api(path) -> decoded table, from the authenticated GitHub REST API.
func (a *api) ghAPI(L *lua.LState) int {
	p := L.CheckString(1)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	var v any
	if _, _, err := a.env.Client.GitHub(a.ctx, p, "", &v); err != nil {
		L.RaiseError("gh.api %s: %v", p, err)
	}
	L.Push(toLua(L, v))
	return 1
}

// url.resolve(base, ref) -> absolute URL
func (a *api) urlResolve(L *lua.LState) int {
	base, err := url.Parse(L.CheckString(1))
	if err != nil {
		L.RaiseError("url.resolve: %v", err)
	}
	ref, err := url.Parse(L.CheckString(2))
	if err != nil {
		L.RaiseError("url.resolve: %v", err)
	}
	L.Push(lua.LString(base.ResolveReference(ref).String()))
	return 1
}

// url.basename(u) -> last path segment, without query string
func (a *api) urlBasename(L *lua.LState) int {
	raw := L.CheckString(1)
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		raw = u.Path
	}
	L.Push(lua.LString(path.Base(raw)))
	return 1
}

func (a *api) logAt(level int) lua.LGFunction {
	return func(L *lua.LState) int {
		parts := make([]string, L.GetTop())
		for i := range parts {
			parts[i] = L.ToStringMeta(L.Get(i + 1)).String()
		}
		fetch.Logf(a.ctx, level, "%s", strings.Join(parts, " "))
		return 0
	}
}

// toLua converts decoded JSON/YAML/jq values to Lua values.
func toLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case string:
		return lua.LString(x)
	case float64:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case uint64:
		return lua.LNumber(x)
	case []any:
		t := L.CreateTable(len(x), 0)
		for _, e := range x {
			t.Append(toLua(L, e))
		}
		return t
	case map[string]any:
		t := L.CreateTable(0, len(x))
		for k, e := range x {
			t.RawSetString(k, toLua(L, e))
		}
		return t
	case map[any]any: // YAML with non-string keys
		t := L.CreateTable(0, len(x))
		for k, e := range x {
			t.RawSetString(fmt.Sprint(k), toLua(L, e))
		}
		return t
	}
	return lua.LString(fmt.Sprint(v)) // big ints, YAML timestamps
}

// fromLua converts to encoding/json and gojq types. A table with keys exactly
// 1..n is a list; any other, including {}, is a map.
func fromLua(v lua.LValue) any {
	switch x := v.(type) {
	case lua.LBool:
		return bool(x)
	case lua.LNumber:
		if f := float64(x); f == float64(int(f)) {
			return int(f)
		}
		return float64(x)
	case lua.LString:
		return string(x)
	case *lua.LTable:
		n, count := x.Len(), 0
		x.ForEach(func(_, _ lua.LValue) { count++ })
		if n > 0 && n == count {
			list := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				list = append(list, fromLua(x.RawGetInt(i)))
			}
			return list
		}
		m := make(map[string]any, count)
		x.ForEach(func(k, e lua.LValue) { m[k.String()] = fromLua(e) })
		return m
	}
	return nil
}
