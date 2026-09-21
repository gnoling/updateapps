package luart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

func testEnv(t *testing.T, handler http.HandlerFunc) (Env, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := fetch.New("tok")
	c.GitHubAPI = srv.URL
	c.Backoff = time.Millisecond
	app := &def.App{ID: "snes9x", Install: def.Install{Dest: "/apps/snes9x"}}
	return Env{Client: c, App: app, Vars: def.Vars{AppDir: "/apps", AppImageDir: "/apps/appimages", Home: "/home/u"}}, srv.URL
}

func run(t *testing.T, env Env, code string) (*Result, error) {
	t.Helper()
	return Resolve(context.Background(), env, "test.lua", code)
}

// expect runs code that must return {version = <anything>, url = "x"} and
// returns the version: a compact way to assert on a computed value.
func expect(t *testing.T, env Env, code, want string) {
	t.Helper()
	res, err := run(t, env, code)
	if err != nil {
		t.Errorf("%s\n-> %v", code, err)
	} else if res.Version != want {
		t.Errorf("%s\n-> %q, want %q", code, res.Version, want)
	}
}

func TestReturnShapes(t *testing.T) {
	env, _ := testEnv(t, nil)
	res, err := run(t, env, `return {version = "1.2", display = "v1.2", url = "https://x/y/app-1.2.tgz?token=1",
		filename = "app.tgz", sha256 = "abc", headers = {Authorization = "Bearer t"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.2" || res.Display != "v1.2" || res.Filename != "app.tgz" || res.SHA256 != "abc" || res.Headers["Authorization"] != "Bearer t" {
		t.Errorf("%+v", res)
	}
	expect(t, env, `return {version = 1234, url = "x"}`, "1234") // numeric build numbers are fine

	var soft *SoftFailure
	if _, err = run(t, env, `return nil, "build still running"`); !errors.As(err, &soft) || !strings.Contains(soft.Reason, "build still running") {
		t.Errorf("nil, reason should be a soft failure: %v", err)
	}
	if _, err = run(t, env, `return nil`); !errors.As(err, &soft) {
		t.Errorf("bare nil should be a soft failure: %v", err)
	}
	for name, code := range map[string]string{
		"missing url":   `return {version = "1"}`,
		"wrong type":    `return "1.2"`,
		"syntax error":  `return {`,
		"runtime error": `local t = nil; return t.x`,
		"error()":       `error("upstream changed their API")`,
	} {
		if _, err := run(t, env, code); err == nil || errors.As(err, &soft) {
			t.Errorf("%s: want a hard error, got %v", name, err)
		} else if strings.Contains(err.Error(), "stack traceback") {
			t.Errorf("%s: Go-side traceback leaked into the message: %v", name, err)
		}
	}
}

func TestSandbox(t *testing.T) {
	env, _ := testEnv(t, nil)
	for _, name := range []string{"io", "package", "debug", "require", "dofile", "loadfile", "os.execute", "os.remove", "os.getenv", "os.exit"} {
		expect(t, env, `return {version = tostring(`+name+`), url = "x"}`, "nil")
	}
	// What scripts do get: the language, the clock, and config.
	expect(t, env, `return {version = string.format("%s-%d", string.upper("ab"), math.max(1, 2)) .. table.concat({"x","y"}, "+"), url = "x"}`, "AB-2x+y")
	expect(t, env, `return {version = tostring(os.time() > 1700000000 and #os.date("%Y%m%d") == 8), url = "x"}`, "true")
	expect(t, env, `return {version = config.name .. "|" .. config.appdir .. "|" .. config.appimagedir .. "|" .. config.dest, url = "x"}`,
		"snes9x|/apps|/apps/appimages|/apps/snes9x")
}

func TestTimeoutStopsRunawayScripts(t *testing.T) {
	old := Timeout
	Timeout = 100 * time.Millisecond
	defer func() { Timeout = old }()
	env, _ := testEnv(t, nil)
	start := time.Now()
	_, err := run(t, env, `while true do end`)
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Errorf("err=%v after %s", err, time.Since(start))
	}
}

func TestHTTP(t *testing.T) {
	env, url := testEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("ETag", `"abc"`)
			fmt.Fprintf(w, "ua=%s", r.Header.Get("X-Test"))
		case "/gone":
			http.Error(w, "no such build", http.StatusNotFound)
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, `{"method":%q,"type":%q,"got":%s}`, r.Method, r.Header.Get("Content-Type"), body)
		case "/raw":
			body, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "%s:%s", r.Method, body)
		}
	})
	expect(t, env, `local body, status, headers = http.get("`+url+`/page", {["X-Test"] = "hi"})
		return {version = body .. "|" .. status .. "|" .. headers.etag, url = "x"}`, `ua=hi|200|"abc"`)
	expect(t, env, `local status, headers = http.head("`+url+`/page") return {version = status .. headers.etag, url = "x"}`, `200"abc"`)
	// A 404 is data the script can act on, not an exception.
	expect(t, env, `local body, status = http.get("`+url+`/gone") return {version = status .. ":" .. body, url = "x"}`, "404:no such build\n")
	expect(t, env, `local r, status = http.post_json("`+url+`/echo", {query = "q", list = {1, 2}, empty = {}})
		return {version = r.method .. "|" .. r.type .. "|" .. r.got.query .. "|" .. #r.got.list .. "|" .. status, url = "x"}`, "POST|application/json|q|2|200")
	expect(t, env, `local body = http.post("`+url+`/raw", "a=1") return {version = body, url = "x"}`, "POST:a=1")

	// A dead host is an error the script didn't have to check for.
	env.Client.MaxRetries = 0
	if _, err := run(t, env, `http.get("http://127.0.0.1:1/") return {version = "1", url = "x"}`); err == nil {
		t.Error("transport failure should fail the script")
	}
}

func TestDataHelpers(t *testing.T) {
	env, _ := testEnv(t, nil)
	expect(t, env, `local t = json.decode('{"a":[1,2,{"b":"deep"}],"n":null,"f":1.5,"id":35385642750}')
		return {version = t.a[3].b .. "|" .. tostring(t.n) .. "|" .. t.f .. "|" .. t.id, url = "x"}`, "deep|nil|1.5|35385642750")
	expect(t, env, `return {version = json.encode({1, 2, 3}) .. json.encode({k = "v"}), url = "x"}`, `[1,2,3]{"k":"v"}`)
	expect(t, env, `local y = yaml.decode("latest:\n  - version: '15.3'\n    name: stable\n")
		return {version = y.latest[1].version, url = "x"}`, "15.3")

	// jq takes a table or a JSON string; jq() is the first result, jq_all() every one.
	expect(t, env, `return {version = jq('{"tasks":[{"name":"a","ok":false},{"name":"b","ok":true}]}', '.tasks[] | select(.ok) | .name'), url = "x"}`, "b")
	expect(t, env, `return {version = table.concat(jq_all({tasks = {{name = "a"}, {name = "b"}}}, ".tasks[].name"), ","), url = "x"}`, "a,b")
	expect(t, env, `return {version = tostring(jq({}, ".missing")), url = "x"}`, "nil")
	if _, err := run(t, env, `return {version = jq({}, ".["), url = "x"}`); err == nil || !strings.Contains(err.Error(), "jq:") {
		t.Errorf("bad jq should name itself: %v", err)
	}

	page := `<a href="/files/app-1.tar.gz">one</a><a class="dl" href="app-2.tar.gz"> two </a>`
	expect(t, env, `local hrefs = html.select([[`+page+`]], "a", "href") return {version = #hrefs .. hrefs[2], url = "x"}`, "2app-2.tar.gz")
	expect(t, env, `return {version = html.select([[`+page+`]], "a.dl")[1], url = "x"}`, "two")
	// S3-style XML listings work with the same call.
	expect(t, env, `local keys = html.select("<ListBucketResult><Contents><Key>b/1.zip</Key></Contents><Contents><Key>b/2.zip</Key></Contents></ListBucketResult>", "contents > key")
		return {version = keys[#keys], url = "x"}`, "b/2.zip")
	if _, err := run(t, env, `html.select("<a>", "a[[[") return {version = "1", url = "x"}`); err == nil {
		t.Error("a bad selector should be an error, not a crash")
	}

	expect(t, env, `return {version = url.resolve("https://site/dl/index.php?x=1", "files/a.tgz") .. "|" .. url.basename("https://site/a/b.tgz?sig=1"), url = "x"}`,
		"https://site/dl/files/a.tgz|b.tgz")
}

func TestGitHubAPIAndLogging(t *testing.T) {
	env, _ := testEnv(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "auth": r.Header.Get("Authorization")})
	})
	var logged []string
	ctx := fetch.WithLogger(context.Background(), func(level int, format string, args ...any) {
		logged = append(logged, fmt.Sprintf("%d:", level)+fmt.Sprintf(format, args...))
	})
	res, err := Resolve(ctx, env, "t.lua", `local r = gh.api("repos/o/r/releases")
		log.info("resolved", r.path) log.warn("heads up") print("via print")
		return {version = r.path .. "|" .. r.auth, url = "x"}`)
	if err != nil || res.Version != "/repos/o/r/releases|Bearer tok" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	got := strings.Join(logged, "\n")
	for _, want := range []string{"1:resolved /repos/o/r/releases", "0:heads up", "1:via print"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
}
