package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/install"
	"github.com/gnoling/updateapps/internal/source"
	"github.com/gnoling/updateapps/internal/state"
	"github.com/gnoling/updateapps/internal/systest"
)

// fakeResolver stands in for a source; downloads still go over real HTTP to
// an httptest server.
type fakeResolver struct {
	url      string
	version  func(id string) string
	delay    time.Duration
	fail     map[string]error
	sha256   string
	sized    bool // report the asset's size, as the GitHub sources do
	running  atomic.Int32
	maxSeen  atomic.Int32
	resolves atomic.Int32
}

func (f *fakeResolver) Resolve(ctx context.Context, _ *fetch.Client, app *def.App, _ *source.Cache) (*source.Result, error) {
	f.resolves.Add(1)
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		seen := f.maxSeen.Load()
		if n <= seen || f.maxSeen.CompareAndSwap(seen, n) {
			break
		}
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := f.fail[app.ID]; err != nil {
		return nil, err
	}
	v := "v1"
	if f.version != nil {
		v = f.version(app.ID)
	}
	res := &source.Result{Version: v, URL: f.url + "/" + app.ID, Filename: app.ID, SHA256: f.sha256}
	if f.sized && strings.HasSuffix(app.ID, ".deb") {
		res.Size = int64(len(debFor(app.ID)))
	}
	return res, nil
}

type harness struct {
	eng       *Engine
	resolver  *fakeResolver
	appdir    string
	path      string // state file
	tmp       string // parent of run scratch dirs
	downloads atomic.Int32
	slow      chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{appdir: t.TempDir(), slow: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/broken":
			http.Error(w, "nope", http.StatusNotFound)
			return
		case r.URL.Path == "/slow":
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			select {
			case <-h.slow:
			case <-r.Context().Done():
			}
			return
		}
		h.downloads.Add(1)
		if strings.HasSuffix(r.URL.Path, ".deb") {
			w.Write(debFor(r.URL.Path[1:]))
			return
		}
		fmt.Fprintf(w, "binary for %s", r.URL.Path[1:])
	}))
	t.Cleanup(srv.Close)

	h.path = filepath.Join(t.TempDir(), "state.json")
	st, err := state.Open(h.path)
	if err != nil {
		t.Fatal(err)
	}
	h.resolver = &fakeResolver{url: srv.URL}
	client := fetch.New("")
	client.Backoff = time.Millisecond
	h.tmp = t.TempDir()
	h.eng = &Engine{Client: client, State: st, StatePath: h.path, TempDir: h.tmp,
		Resolvers: map[string]source.Resolver{"fake": h.resolver}}
	return h
}

func (h *harness) apps(ids ...string) []*def.App {
	var out []*def.App
	for _, id := range ids {
		out = append(out, &def.App{ID: id, Name: id, Source: def.Source{Type: "fake"},
			Install: def.Install{Type: def.InstallFile, Dest: filepath.Join(h.appdir, id)}})
	}
	return out
}

func numbered(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("app%02d", i)
	}
	return ids
}

// run drains a run and returns each app's final event plus the summary.
func (h *harness) run(t *testing.T, ctx context.Context, apps []*def.App, opts Options) (map[string]Event, Summary) {
	t.Helper()
	events, err := h.eng.Run(ctx, apps, opts)
	if err != nil {
		t.Fatal(err)
	}
	finals := map[string]Event{}
	var sum Summary
	for ev := range events {
		if ev.Final {
			if _, dup := finals[ev.AppID]; dup {
				t.Errorf("%s got two final events", ev.AppID)
			}
			finals[ev.AppID] = ev
		}
		if ev.Kind == RunFinished {
			sum = *ev.Summary
		}
	}
	return finals, sum
}

func (h *harness) reopened(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(h.path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestUpdateInstallsAndRecords(t *testing.T) {
	h := newHarness(t)
	finals, sum := h.run(t, context.Background(), h.apps("a", "b"), Options{Jobs: 4})
	if sum.Updated != 2 || sum.Failed != 0 || finals["a"].Kind != Installed {
		t.Fatalf("sum=%+v finals=%+v", sum, finals)
	}
	if body, _ := os.ReadFile(filepath.Join(h.appdir, "a")); string(body) != "binary for a" {
		t.Errorf("installed file = %q", body)
	}
	if finals["a"].Version != "v1" {
		t.Errorf("a source that sets no display version should show its version, got %q", finals["a"].Version)
	}
	if e := h.reopened(t).Get("a"); e.Version != "v1" || e.Display != "v1" || e.InstalledAt == nil || e.LastChecked == nil {
		t.Errorf("state = %+v", e)
	}

	// Second run: nothing to do, nothing downloaded.
	finals, sum = h.run(t, context.Background(), h.apps("a", "b"), Options{Jobs: 4})
	if sum.Updated != 0 || sum.UpToDate != 2 || finals["a"].Kind != UpToDate {
		t.Errorf("second run: %+v", sum)
	}
	// --force reinstalls and still records.
	_, sum = h.run(t, context.Background(), h.apps("a"), Options{Jobs: 4, Force: true})
	if sum.Updated != 1 {
		t.Errorf("force: %+v", sum)
	}
}

func TestConcurrencyCapRespected(t *testing.T) {
	h := newHarness(t)
	h.resolver.delay = 20 * time.Millisecond
	_, sum := h.run(t, context.Background(), h.apps(numbered(24)...), Options{Jobs: 3})
	if sum.Updated != 24 {
		t.Fatalf("sum = %+v", sum)
	}
	if got := h.resolver.maxSeen.Load(); got != 3 {
		t.Errorf("max concurrent jobs = %d, want exactly 3 (cap respected and actually parallel)", got)
	}
}

// The second bash bug: versions were recorded before the download, so a failed
// install looked current. Here a failure must leave the version unrecorded.
func TestFailureRecordsNoVersion(t *testing.T) {
	h := newHarness(t)
	apps := h.apps("good", "unresolvable", "undownloadable")
	h.resolver.fail = map[string]error{"unresolvable": errors.New("no asset matches")}
	apps[2].ID = "broken" // resolver URL becomes /broken -> 404
	apps[2].Name = "broken"

	finals, sum := h.run(t, context.Background(), apps, Options{Jobs: 4})
	if sum.Updated != 1 || sum.Failed != 2 {
		t.Fatalf("sum = %+v", sum)
	}
	st := h.reopened(t)
	for _, id := range []string{"unresolvable", "broken"} {
		e := st.Get(id)
		if finals[id].Kind != Failed || e.Version != "" || e.InstalledAt != nil || e.LastError == "" {
			t.Errorf("%s: final=%v state=%+v", id, finals[id].Kind, e)
		}
	}
	if _, err := os.Stat(filepath.Join(h.appdir, "undownloadable")); err == nil {
		t.Error("a failed download left a file at dest")
	}

	// Next run retries by itself, and success clears the error.
	h.resolver.fail = nil
	_, sum = h.run(t, context.Background(), apps[:2], Options{Jobs: 4})
	if e := h.reopened(t).Get("unresolvable"); sum.Updated != 1 || e.Version != "v1" || e.LastError != "" {
		t.Errorf("retry: sum=%+v state=%+v", sum, e)
	}
}

func TestSoftFailureIsSkippedNotFailed(t *testing.T) {
	h := newHarness(t)
	h.resolver.fail = map[string]error{"rolling": &source.SoftError{Msg: "release mid-recreate"}}
	finals, sum := h.run(t, context.Background(), h.apps("rolling"), Options{Jobs: 1})
	if finals["rolling"].Kind != Skipped || sum.Failed != 0 || sum.Skipped != 1 {
		t.Errorf("final=%v sum=%+v", finals["rolling"].Kind, sum)
	}
}

func TestCheckAndDryRunTouchNothing(t *testing.T) {
	h := newHarness(t)
	finals, sum := h.run(t, context.Background(), h.apps("a"), Options{Jobs: 1, Mode: ModeCheck})
	if finals["a"].Kind != NewVersion || sum.Updated != 1 {
		t.Fatalf("check: %+v", sum)
	}
	if e := h.reopened(t).Get("a"); e.Version != "" || e.LastChecked == nil {
		t.Errorf("check should record last_checked only: %+v", e)
	}

	os.Remove(h.path)
	finals, _ = h.run(t, context.Background(), h.apps("b"), Options{Jobs: 1, DryRun: true})
	if finals["b"].Kind != NewVersion || finals["b"].Message == "" {
		t.Errorf("dry run final = %+v", finals["b"])
	}
	if _, err := os.Stat(h.path); err == nil {
		t.Error("dry run wrote the state file")
	}
	if entries, _ := os.ReadDir(h.appdir); len(entries) != 0 {
		t.Errorf("check/dry-run put files in appdir: %v", entries)
	}
}

func TestCancelCleansUpAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	defer close(h.slow)
	apps := h.apps(numbered(6)...)
	for _, a := range apps {
		a.ID = "slow" // every download stalls mid-transfer
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := h.eng.Run(ctx, apps, Options{Jobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	var sum Summary
	stopped := 0
	for ev := range events {
		if ev.Kind == Downloading {
			cancel() // Ctrl-C once a transfer is underway
		}
		if ev.Final && ev.Kind == Skipped && ev.Message == "cancelled" {
			stopped++
		}
		if ev.Kind == RunFinished {
			sum = *ev.Summary
		}
	}
	if !sum.Cancelled || sum.Failed != 0 || stopped == 0 || stopped > 2 {
		t.Errorf("sum=%+v stopped=%d (only the 2 in-flight jobs should report, and not as failures)", sum, stopped)
	}
	if got := h.resolver.resolves.Load(); got > 2 {
		t.Errorf("%d jobs started; queued jobs should be dropped on cancel", got)
	}
	if e := h.reopened(t).Get("slow"); e.Version != "" || e.LastError != "" {
		t.Errorf("cancel recorded state: %+v", e)
	}
	if entries, _ := os.ReadDir(h.appdir); len(entries) != 0 {
		t.Errorf("staging left in appdir after cancel: %v", entries)
	}
	if left, _ := os.ReadDir(h.tmp); len(left) != 0 {
		t.Errorf("run temp dir left behind: %v", left)
	}
}

func TestRunLock(t *testing.T) {
	h := newHarness(t)
	h.resolver.delay = 200 * time.Millisecond
	first, err := h.eng.Run(context.Background(), h.apps("a"), Options{Jobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	// A second front end (say the GUI) sharing this state file.
	st2, _ := state.Open(h.path)
	other := &Engine{Client: h.eng.Client, State: st2, StatePath: h.path, Resolvers: h.eng.Resolvers}
	if _, err := other.Run(context.Background(), h.apps("b"), Options{Jobs: 1}); !errors.Is(err, ErrLocked) {
		t.Errorf("concurrent run: err = %v, want ErrLocked", err)
	}
	// Dry runs write nothing, so they don't need the lock.
	if ev, err := other.Run(context.Background(), h.apps("b"), Options{Jobs: 1, DryRun: true}); err != nil {
		t.Errorf("dry run during another run: %v", err)
	} else {
		for range ev {
		}
	}
	for range first {
	}
	if ev, err := other.Run(context.Background(), h.apps("b"), Options{Jobs: 1}); err != nil {
		t.Errorf("lock not released after the run: %v", err)
	} else {
		for range ev {
		}
	}
}

func TestUnimplementedSourceFailsCleanly(t *testing.T) {
	h := newHarness(t)
	apps := h.apps("scripted")
	apps[0].Source.Type = def.SourceScript
	_, sum := h.run(t, context.Background(), apps, Options{Jobs: 1})
	if sum.Failed != 1 || h.resolver.resolves.Load() != 0 {
		t.Errorf("sum=%+v", sum)
	}
}

// Events for one app arrive in pipeline order, and every job's log travels
// on its own final event, so a front end can't interleave blocks.
func TestEventOrderAndPerJobLogs(t *testing.T) {
	h := newHarness(t)
	events, err := h.eng.Run(context.Background(), h.apps(numbered(10)...), Options{Jobs: 5})
	if err != nil {
		t.Fatal(err)
	}
	seq := map[string][]EventKind{}
	for ev := range events {
		if ev.AppID == "" {
			continue
		}
		if n := len(seq[ev.AppID]); n == 0 || seq[ev.AppID][n-1] != ev.Kind {
			seq[ev.AppID] = append(seq[ev.AppID], ev.Kind)
		}
		if ev.Final {
			for _, l := range ev.Log {
				if strings.HasPrefix(l.Text, "asset") && !strings.Contains(l.Text, "/"+ev.AppID+")") {
					t.Errorf("%s's block carries another job's log line: %s", ev.AppID, l.Text)
				}
			}
		}
	}
	want := []EventKind{Queued, Checking, NewVersion, Downloading, Installed}
	for id, got := range seq {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: events %v, want %v", id, got, want)
		}
	}
}

// A failing post hook fails the app: nothing is recorded, so the next run
// redoes the install and the hooks (bash needed hand-written un-recording).
func TestPostHookFailureIsNotRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	h := newHarness(t)
	h.eng.Vars = def.Vars{AppDir: h.appdir}
	marker := filepath.Join(h.appdir, "deployed")
	apps := h.apps("svc")
	apps[0].Post = []string{`test "$VERSION" = v1 && test -f "$FILE" && exit 9`, "touch " + marker}

	finals, sum := h.run(t, context.Background(), apps, Options{Jobs: 1})
	if sum.Failed != 1 || !strings.Contains(finals["svc"].Message, "post hook 1 failed") {
		t.Fatalf("sum=%+v final=%+v", sum, finals["svc"])
	}
	if e := h.reopened(t).Get("svc"); e.Version != "" {
		t.Errorf("version recorded despite failed hook: %+v", e)
	}

	apps[0].Post = []string{"touch " + marker}
	finals, sum = h.run(t, context.Background(), apps, Options{Jobs: 1})
	if _, err := os.Stat(marker); err != nil || sum.Updated != 1 || h.reopened(t).Get("svc").Version != "v1" {
		t.Errorf("retry: sum=%+v err=%v", sum, err)
	}
	var sawHooks bool
	for _, l := range finals["svc"].Log {
		sawHooks = sawHooks || strings.HasPrefix(l.Text, "post[1]")
	}
	if !sawHooks {
		t.Error("hook commands should appear in the job log")
	}
}

func TestInstallNoneHandsFileToHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	h := newHarness(t)
	out := filepath.Join(h.appdir, "received")
	apps := h.apps("jar")
	apps[0].Install = def.Install{Type: def.InstallNone}
	apps[0].Post = []string{`cp "$FILE" ` + out + `; basename "$FILE" >> ` + out}
	if _, sum := h.run(t, context.Background(), apps, Options{Jobs: 1}); sum.Updated != 1 {
		t.Fatalf("sum=%+v", sum)
	}
	// The staged download keeps its upstream file name.
	if body, _ := os.ReadFile(out); string(body) != "binary for jarjar\n" {
		t.Errorf("hook saw %q", body)
	}
}

func TestDigestMismatchBlocksInstall(t *testing.T) {
	h := newHarness(t)
	h.resolver.sha256 = strings.Repeat("0", 64)
	finals, sum := h.run(t, context.Background(), h.apps("a"), Options{Jobs: 1})
	if sum.Failed != 1 || !strings.Contains(finals["a"].Message, "sha256 mismatch") {
		t.Fatalf("sum=%+v final=%+v", sum, finals["a"])
	}
	if _, err := os.Stat(filepath.Join(h.appdir, "a")); err == nil {
		t.Error("a file that failed verification was installed")
	}
}

func TestMarkCurrent(t *testing.T) {
	h := newHarness(t)
	apps := h.apps("have", "missing")
	os.WriteFile(filepath.Join(h.appdir, "have"), []byte("installed by the old bash script"), 0o755)

	finals, sum := h.run(t, context.Background(), apps, Options{Jobs: 2, Mode: ModeMarkCurrent})
	if finals["have"].Kind != Marked || finals["missing"].Kind != Skipped || sum.Updated != 1 || sum.Skipped != 1 {
		t.Fatalf("sum=%+v finals=%v/%v", sum, finals["have"].Kind, finals["missing"].Kind)
	}
	if body, _ := os.ReadFile(filepath.Join(h.appdir, "have")); string(body) != "installed by the old bash script" {
		t.Error("mark-current must not download anything")
	}

	// Now an update leaves the adopted app alone and installs the missing one.
	finals, sum = h.run(t, context.Background(), apps, Options{Jobs: 2})
	if finals["have"].Kind != UpToDate || finals["missing"].Kind != Installed {
		t.Errorf("after marking: have=%v missing=%v sum=%+v", finals["have"].Kind, finals["missing"].Kind, sum)
	}
}

// debFor builds the package a test app id names: "bat_0.25.0_amd64.deb".
func debFor(id string) []byte {
	parts := strings.Split(strings.TrimSuffix(id, ".deb"), "_")
	return systest.Deb(strings.TrimSuffix(parts[0], "-broken"), parts[1], parts[2])
}

func (h *harness) debApps(ids ...string) []*def.App {
	apps := h.apps(ids...)
	for _, a := range apps {
		a.Install = def.Install{Type: def.InstallDeb}
	}
	return apps
}

// System packages download in parallel with everything else, then install in
// one transaction at the end of the run: one password prompt, however many.
func TestDebsAreBatchedAtTheEndOfTheRun(t *testing.T) {
	fake := systest.Install(t)
	h := newHarness(t)
	h.eng.System = install.System{PendingDir: filepath.Join(h.tmp, "pending")}
	apps := append(h.debApps("bat_0.25.0_amd64.deb", "fd_10.2.0_amd64.deb"), h.apps("plain")...)

	events, err := h.eng.Run(context.Background(), apps, Options{Jobs: 4})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	var sum Summary
	for ev := range events {
		switch {
		case ev.Kind == Finalizing:
			order = append(order, "FINALIZING "+ev.Message)
		case ev.Kind == Pending || ev.Final:
			order = append(order, ev.Kind.String()+" "+ev.AppID)
		case ev.Kind == RunFinished:
			sum = *ev.Summary
		}
	}
	got := strings.Join(order, "\n")
	fin := strings.Index(got, "FINALIZING Installing 2 system packages: ")
	if fin < 0 || strings.Index(got, "Installed plain") > fin || strings.Count(got[:fin], "Pending ") != 2 || strings.Count(got[fin:], "Installed ") != 2 {
		t.Errorf("want both debs pending and the ordinary app done before one finalize step, then both installed:\n%s", got)
	}
	debArgs := 0
	for _, arg := range strings.Fields(strings.Join(fake.Calls("apt-get"), " ")) {
		if strings.HasSuffix(arg, ".deb") {
			debArgs++
		}
	}
	if calls := fake.Calls("apt-get"); len(calls) != 1 || debArgs != 2 {
		t.Errorf("apt-get calls = %q, want one transaction with both packages", calls)
	}
	if sum.Updated != 3 || sum.Failed != 0 {
		t.Errorf("sum = %+v", sum)
	}
	if e := h.reopened(t).Get("bat_0.25.0_amd64.deb"); e.Version != "v1" || e.InstalledAt == nil {
		t.Errorf("state = %+v", e)
	}
}

// No root available: not a failure and nothing is recorded. The download is
// kept, and the next run (root available now) uses it instead of fetching again.
func TestDebWithoutRootThenWithIt(t *testing.T) {
	fake := systest.Install(t)
	fake.Deny()
	h := newHarness(t)
	h.resolver.sized = true
	h.eng.System = install.System{PendingDir: filepath.Join(h.tmp, "pending")}
	apps := h.debApps("bat_0.25.0_amd64.deb")

	finals, sum := h.run(t, context.Background(), apps, Options{Jobs: 1})
	final := finals["bat_0.25.0_amd64.deb"]
	if final.Kind != ActionNeeded || !strings.Contains(final.Message, "sudo apt-get install ") || sum.Action != 1 || sum.Failed != 0 {
		t.Fatalf("final=%+v sum=%+v", final, sum)
	}
	if e := h.reopened(t).Get("bat_0.25.0_amd64.deb"); e.Version != "" {
		t.Errorf("recorded without being installed: %+v", e)
	}

	fake.Allow()
	before := h.downloads.Load()
	finals, _ = h.run(t, context.Background(), apps, Options{Jobs: 1})
	if finals["bat_0.25.0_amd64.deb"].Kind != Installed || h.downloads.Load() != before {
		t.Errorf("second run: kind=%v, downloads %d -> %d (the kept file should be reused)", finals["bat_0.25.0_amd64.deb"].Kind, before, h.downloads.Load())
	}
	if left, _ := os.ReadDir(filepath.Join(h.tmp, "pending")); len(left) != 0 {
		t.Errorf("kept download should be removed once installed: %v", left)
	}
	if fake.DpkgVersion("bat") != "0.25.0" {
		t.Error("not installed")
	}
}

func TestDebPostHooksRunAfterTheBatch(t *testing.T) {
	systest.Install(t)
	h := newHarness(t)
	h.eng.System = install.System{PendingDir: filepath.Join(h.tmp, "pending")}
	marker := filepath.Join(h.appdir, "hooked")
	apps := h.debApps("bat_0.25.0_amd64.deb")
	apps[0].Post = []string{`echo "$VERSION" > ` + marker}
	if _, sum := h.run(t, context.Background(), apps, Options{Jobs: 1}); sum.Updated != 1 {
		t.Fatalf("sum=%+v", sum)
	}
	if body, _ := os.ReadFile(marker); strings.TrimSpace(string(body)) != "v1" {
		t.Errorf("hook output = %q", body)
	}
}

// A flatpak updated outside updateapps (plain `flatpak update`) isn't
// "updated" again: the system says it already has the latest.
func TestAlreadyLatestOnTheSystemIsJustRecorded(t *testing.T) {
	h := newHarness(t)
	h.eng.Resolvers["sys"] = resolverFunc(func(*def.App) (*source.Result, error) {
		return &source.Result{Version: "commit-b", Display: "2.0", Installed: "commit-b"}, nil
	})
	apps := h.apps("fp")
	apps[0].Source.Type, apps[0].Install = "sys", def.Install{Type: def.InstallFlatpak}
	h.eng.State.Update("fp", func(e *state.Entry) { e.Version = "commit-a" })

	finals, sum := h.run(t, context.Background(), apps, Options{Jobs: 1})
	if finals["fp"].Kind != UpToDate || sum.Updated != 0 || h.reopened(t).Get("fp").Version != "commit-b" {
		t.Errorf("final=%v sum=%+v state=%+v", finals["fp"].Kind, sum, h.reopened(t).Get("fp"))
	}
}

type resolverFunc func(*def.App) (*source.Result, error)

func (f resolverFunc) Resolve(_ context.Context, _ *fetch.Client, app *def.App, _ *source.Cache) (*source.Result, error) {
	return f(app)
}

// Naming a blocked app explicitly doesn't get around its repository's lack of
// trust; it gets an explanation instead.
func TestBlockedAppIsRefusedWithAReason(t *testing.T) {
	h := newHarness(t)
	apps := h.apps("sneaky")
	apps[0].Blocked = "it runs shell commands (post), and repository theirs isn't trusted"
	finals, sum := h.run(t, context.Background(), apps, Options{Jobs: 1})
	if finals["sneaky"].Kind != Skipped || !strings.Contains(finals["sneaky"].Message, "isn't trusted") || sum.Failed != 0 || h.resolver.resolves.Load() != 0 {
		t.Errorf("final=%+v sum=%+v resolves=%d", finals["sneaky"], sum, h.resolver.resolves.Load())
	}
}
