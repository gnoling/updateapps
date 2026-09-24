package gui

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/gnoling/updateapps/internal/cli"
	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/engine"
	"github.com/gnoling/updateapps/internal/repo"
	"github.com/gnoling/updateapps/internal/state"
)

//go:embed icon.svg
var iconSVG []byte

var icon = fyne.NewStaticResource("updateapps.svg", iconSVG)

// Options come from the command line.
type Options struct {
	Config string // config file; "" = the default
	Defs   string // one flat trusted folder instead of the repositories (--defs)
	Hidden bool   // start in the tray without showing the window
}

// ui is the window and everything behind it. Widgets are only touched on the
// UI goroutine: callbacks run there, and goroutines hand work back with
// fyne.Do.
type ui struct {
	app  fyne.App
	win  fyne.Window
	opts Options

	cfg   *config.Config
	set   *repo.Set
	model Model

	list     *widget.List
	visible  []*row
	selected string // app id
	search   *widget.Entry
	category *widget.Select
	show     *widget.Select
	details  *fyne.Container
	logGrid  *widget.TextGrid
	logView  *container.Scroll
	logText  strings.Builder
	logMu    sync.Mutex // logText is appended from goroutines
	status   *widget.Label

	checkAll, updateAll, updateChecked, cancelBtn *widget.Button

	running  bool
	cancel   func()
	lastRun  string
	trayUp   bool
	shown    bool
	trayMenu *fyne.Menu

	timer      *time.Timer // the next scheduled check
	timerArmed bool
	announced  map[string]bool // ids whose update a notification has mentioned
}

// Main runs the GUI until it quits.
func Main(opts Options) int {
	a := app.NewWithID("io.github.gnoling.updateapps")
	a.SetIcon(icon)
	u := &ui{app: a, opts: opts, announced: map[string]bool{}}
	u.build()
	u.setupTray()
	go u.reload(nil)
	if opts.Hidden && u.trayUp {
		u.app.Run()
	} else {
		u.showWindow()
		u.app.Run()
	}
	return 0
}

func (u *ui) showWindow() {
	u.shown = true
	u.win.Show()
	u.win.RequestFocus()
}

func (u *ui) hideWindow() {
	u.shown = false
	u.win.Hide()
}

func (u *ui) quit() {
	if u.running {
		dialog.NewConfirm("Quit", "A run is in progress. Quit anyway? Nothing half-done is recorded.", func(ok bool) {
			if ok {
				u.cancel()
				u.app.Quit()
			}
		}, u.win).Show()
		return
	}
	u.app.Quit()
}

func (u *ui) build() {
	u.win = u.app.NewWindow("updateapps")
	u.win.SetIcon(icon)
	u.win.Resize(fyne.NewSize(1150, 720))
	u.win.SetCloseIntercept(func() {
		if u.trayUp {
			u.hideWindow()
		} else {
			u.quit()
		}
	})

	u.checkAll = widget.NewButtonWithIcon("Check all", theme.SearchIcon(), func() { u.run(engine.ModeCheck, u.model.Runnable(), fromWindow) })
	u.updateAll = widget.NewButtonWithIcon("Update all", theme.DownloadIcon(), func() { u.run(engine.ModeUpdate, u.model.Runnable(), fromWindow) })
	u.updateChecked = widget.NewButtonWithIcon("Update checked", theme.ConfirmIcon(), func() { u.run(engine.ModeUpdate, u.model.Checked(), fromWindow) })
	u.cancelBtn = widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() {
		if u.cancel != nil {
			u.cancel()
		}
	})
	u.cancelBtn.Importance = widget.DangerImportance

	u.search = widget.NewEntry()
	u.search.SetPlaceHolder("Search id, name or upstream")
	u.search.OnChanged = func(s string) { u.model.Search = s; u.refreshList() }
	// Callbacks go on after the initial selection: the list doesn't exist yet.
	u.category = widget.NewSelect([]string{"All categories"}, nil)
	u.category.SetSelectedIndex(0)
	u.category.OnChanged = func(s string) {
		u.model.Category = ""
		if s != "All categories" {
			u.model.Category = s
		}
		u.refreshList()
	}
	u.show = widget.NewSelect(showOptions, nil)
	u.show.SetSelectedIndex(0)
	u.model.Show = showAll
	u.show.OnChanged = func(s string) { u.model.Show = s; u.refreshList() }
	toolbar := container.NewBorder(nil, nil,
		container.NewHBox(u.checkAll, u.updateAll, u.updateChecked, u.cancelBtn),
		container.NewHBox(fixedWidth(190, u.category), fixedWidth(210, u.show)),
		u.search)

	u.list = widget.NewList(func() int { return len(u.visible) }, newRowWidget, u.updateRowWidget)
	u.list.OnSelected = func(id widget.ListItemID) {
		if id < len(u.visible) {
			u.selected = u.visible[id].App.ID
			u.renderDetails()
		}
	}
	u.list.OnUnselected = func(widget.ListItemID) {
		u.selected = ""
		u.renderDetails()
	}
	header := newHeader(func(on bool) {
		for _, r := range u.visible {
			r.Checked = on
		}
		u.list.Refresh()
		u.updateButtons()
	})
	table := container.NewBorder(header, nil, nil, nil, u.list)

	u.details = container.NewVBox()
	u.renderDetails()
	// Vertical only, so wrapped text fits the pane's width.
	main := container.NewHSplit(table, container.NewVScroll(container.NewPadded(u.details)))
	main.SetOffset(0.62)

	u.logGrid = widget.NewTextGrid()
	u.logView = container.NewScroll(u.logGrid)
	body := container.NewVSplit(main, u.logView)
	body.SetOffset(0.74)

	u.status = widget.NewLabel("Loading…")
	u.status.Truncation = fyne.TextTruncateEllipsis
	u.win.SetContent(container.NewBorder(container.NewPadded(toolbar), u.status, nil, nil, body))
	u.win.SetMainMenu(u.mainMenu())
	u.updateButtons()
}

func (u *ui) mainMenu() *fyne.MainMenu {
	file := fyne.NewMenu("File",
		fyne.NewMenuItem("Reload definitions", func() { go u.reload(nil) }),
		fyne.NewMenuItem("Pull repositories", func() { go u.pull(nil, nil) }),
		fyne.NewMenuItem("Repositories…", u.reposDialog),
		fyne.NewMenuItem("Settings…", u.settingsDialog),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", u.quit),
	)
	run := fyne.NewMenu("Run",
		fyne.NewMenuItem("Check all", func() { u.run(engine.ModeCheck, u.model.Runnable(), fromWindow) }),
		fyne.NewMenuItem("Update all", func() { u.run(engine.ModeUpdate, u.model.Runnable(), fromWindow) }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Check checked", func() { u.run(engine.ModeCheck, u.model.Checked(), fromWindow) }),
		fyne.NewMenuItem("Update checked", func() { u.run(engine.ModeUpdate, u.model.Checked(), fromWindow) }),
		fyne.NewMenuItem("Mark checked as current", func() { u.run(engine.ModeMarkCurrent, u.model.Checked(), fromWindow) }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Cancel", func() {
			if u.cancel != nil {
				u.cancel()
			}
		}),
	)
	help := fyne.NewMenu("Help", fyne.NewMenuItem("About", u.about))
	return fyne.NewMainMenu(file, run, help)
}

func (u *ui) about() {
	cfgPath, defs := "", ""
	if u.cfg != nil {
		cfgPath, defs = u.cfg.Path, u.cfg.Definitions
		if u.cfg.DefsOverride != "" {
			defs = u.cfg.DefsOverride + " (--defs)"
		}
	}
	dialog.NewInformation("updateapps", "Keeps locally installed apps up to date from their upstream releases.\n\n"+
		"Config: "+cfgPath+"\nDefinitions: "+defs+"\n\nThe updateapps command shares this config, these definitions and the same state.", u.win).Show()
}

// load reads everything from disk. It runs off the UI goroutine and touches
// no widgets; apply puts the result on screen.
func (u *ui) load() (*config.Config, *repo.Set, *state.Store, error) {
	cfg, err := config.Load(u.opts.Config)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg.DefsOverride = u.opts.Defs
	// A url: repository nobody has fetched yet: fetch it now, as the CLI does.
	var missing []string
	for _, r := range repo.Repositories(cfg) {
		if _, err := os.Stat(repo.Dir(cfg, r)); r.URL != "" && os.IsNotExist(err) {
			missing = append(missing, r.Name)
		}
	}
	if len(missing) > 0 {
		u.pullSync(cfg, missing)
	}
	set := repo.Load(cfg)
	st, err := state.Open(cfg.State)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, set, st, nil
}

// reload reads config, definitions and state again, then runs then (if any)
// on the UI goroutine.
func (u *ui) reload(then func()) {
	cfg, set, st, err := u.load()
	fyne.Do(func() {
		if err != nil {
			u.status.SetText("Error: " + err.Error())
			dialog.NewError(err, u.win).Show()
			return
		}
		u.apply(cfg, set, st)
		if then != nil {
			then()
		}
	})
}

func (u *ui) apply(cfg *config.Config, set *repo.Set, st *state.Store) {
	u.cfg, u.set = cfg, set
	u.model.Reset(set.Apps, st)
	for _, e := range set.Errors {
		u.logf("definitions: %v", e)
	}
	options := append([]string{"All categories"}, u.model.Categories()...)
	current := u.category.Selected
	u.category.Options = options
	u.category.Refresh()
	if !contains(options, current) {
		u.category.SetSelectedIndex(0)
	}
	u.refreshList()
	u.updateStatus()
	u.armTimer()
}

// fixedWidth sizes a widget by width, not by its longest option.
func fixedWidth(w float32, obj fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(w, obj.MinSize().Height), obj)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// refreshList reapplies the filters and keeps the selection if it's still
// visible.
func (u *ui) refreshList() {
	u.visible = u.model.Visible()
	u.list.Refresh()
	at := -1
	for i, r := range u.visible {
		if r.App.ID == u.selected {
			at = i
		}
	}
	if at < 0 {
		u.list.UnselectAll()
		if u.selected != "" {
			u.selected = ""
			u.renderDetails()
		}
	} else {
		u.list.Select(at)
	}
	u.updateButtons()
}

func (u *ui) updateButtons() {
	n := len(u.model.Checked())
	u.updateChecked.SetText(fmt.Sprintf("Update checked (%d)", n))
	for _, b := range []*widget.Button{u.checkAll, u.updateAll, u.updateChecked} {
		b.Enable()
	}
	u.cancelBtn.Disable()
	switch {
	case u.running:
		for _, b := range []*widget.Button{u.checkAll, u.updateAll, u.updateChecked} {
			b.Disable()
		}
		u.cancelBtn.Enable()
	case n == 0:
		u.updateChecked.Disable()
	}
}

func (u *ui) updateStatus() {
	if u.cfg == nil {
		return
	}
	var from string
	if u.cfg.DefsOverride != "" {
		from = u.cfg.DefsOverride
	} else {
		var names []string
		for _, r := range u.set.Repos {
			if r.Apps > 0 || r.Name != repo.Local {
				names = append(names, r.Name)
			}
		}
		from = strings.Join(names, ", ")
	}
	text := fmt.Sprintf("%d definitions (%s), %d enabled", len(u.model.Rows), from, len(u.model.Runnable()))
	if u.running {
		text += " · running"
	} else if u.lastRun != "" {
		text += " · " + u.lastRun
	}
	u.status.SetText(text)
}

// logf appends a line to the log pane. Safe from any goroutine.
func (u *ui) logf(format string, args ...any) {
	u.logMu.Lock()
	u.logText.WriteString(fmt.Sprintf(format, args...) + "\n")
	u.logMu.Unlock()
	fyne.Do(u.flushLog)
}

// flushLog puts the log buffer on screen; UI goroutine only.
func (u *ui) flushLog() {
	u.logMu.Lock()
	text := u.logText.String()
	u.logMu.Unlock()
	u.logGrid.SetText(text)
	u.logView.ScrollToBottom()
}

// logWriter lets the CLI's renderer write its blocks into the log pane.
type logWriter struct{ u *ui }

func (w logWriter) Write(p []byte) (int, error) {
	w.u.logMu.Lock()
	w.u.logText.Write(p)
	w.u.logMu.Unlock()
	return len(p), nil
}

func (u *ui) renderer(verbose int, mode engine.Mode, dryRun bool) *cli.Renderer {
	return &cli.Renderer{Out: logWriter{u}, Verbose: verbose, Mode: mode, DryRun: dryRun}
}

// pick maps ids back onto a freshly loaded set.
func pick(apps []*def.App, ids []string) []*def.App {
	byID := map[string]*def.App{}
	for _, a := range apps {
		byID[a.ID] = a
	}
	var out []*def.App
	for _, id := range ids {
		if a := byID[id]; a != nil {
			out = append(out, a)
		}
	}
	return out
}
