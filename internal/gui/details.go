package gui

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/gnoling/updateapps/internal/cli"
	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/engine"
	"github.com/gnoling/updateapps/internal/repo"
)

func wrapped(s string) *widget.Label {
	l := widget.NewLabel(s)
	l.Wrapping = fyne.TextWrapWord
	return l
}

// field is one "Label: value" line that wraps.
func field(label, value string) fyne.CanvasObject {
	t := widget.NewRichText(
		&widget.TextSegment{Text: label + ": ", Style: widget.RichTextStyle{TextStyle: fyne.TextStyle{Bold: true}, Inline: true}},
		&widget.TextSegment{Text: value, Style: widget.RichTextStyle{Inline: true}},
	)
	t.Wrapping = fyne.TextWrapWord
	return t
}

func heading(s string) *widget.RichText {
	return widget.NewRichText(&widget.TextSegment{Text: s, Style: widget.RichTextStyleHeading})
}

func subheading(s string) *widget.RichText {
	return widget.NewRichText(&widget.TextSegment{Text: s, Style: widget.RichTextStyleSubHeading})
}

// renderDetails rebuilds the pane for the selected app.
func (u *ui) renderDetails() {
	var r *row
	if u.selected != "" {
		r = u.model.byID[u.selected]
	}
	if r == nil {
		hint := wrapped("Select an app to see its details.")
		hint.Importance = widget.LowImportance
		u.details.Objects = []fyne.CanvasObject{hint}
		u.details.Refresh()
		return
	}
	a := r.App
	objs := []fyne.CanvasObject{heading(a.Name)}
	meta := a.ID
	if a.Repo != "" {
		meta += " · repository " + a.Repo
	}
	if a.Category != "" {
		meta += " · " + a.Category
	}
	sub := wrapped(meta)
	sub.Importance = widget.LowImportance
	objs = append(objs, sub)
	if a.Description != "" {
		objs = append(objs, wrapped(a.Description))
	}

	enabled := widget.NewCheck("Enabled", func(on bool) { u.setEnabled(a, on) })
	enabled.Checked = a.IsEnabled()
	if u.cfg == nil || u.cfg.DefsOverride != "" {
		enabled.Disable()
	}
	objs = append(objs, enabled)
	if a.Blocked != "" {
		warn := wrapped("Won't run: " + a.Blocked + ". Grant trust under File → Repositories.")
		warn.Importance = widget.WarningImportance
		objs = append(objs, warn)
	}

	check := widget.NewButtonWithIcon("Check", theme.SearchIcon(), func() { u.run(engine.ModeCheck, []*def.App{a}, fromWindow) })
	update := widget.NewButtonWithIcon("Update", theme.DownloadIcon(), func() { u.run(engine.ModeUpdate, []*def.App{a}, fromWindow) })
	folder := widget.NewButtonWithIcon("Open folder", theme.FolderOpenIcon(), func() { u.openFolder(a) })
	edit := widget.NewButtonWithIcon("Edit definition", theme.DocumentCreateIcon(), func() { u.editDefinition(a) })
	if u.running {
		check.Disable()
		update.Disable()
	}
	if dir := installDir(a); dir == "" {
		folder.Disable()
	}
	objs = append(objs, container.NewGridWithColumns(2, check, update, folder, edit))

	for _, f := range []struct{ label, value string }{
		{"Source", sourceSummary(a)},
		{"Installs", installSummary(a)},
		{"Recorded", r.Version()},
		{"Installed", when(r.Entry.InstalledAt)},
		{"Checked", when(r.Entry.LastChecked)},
	} {
		objs = append(objs, field(f.label, f.value))
	}

	if r.Entry.LastError != "" {
		l := wrapped(r.Entry.LastError)
		l.Importance = widget.DangerImportance
		objs = append(objs, subheading("Last error"), l)
	}
	if r.Entry.LastNotice != "" {
		l := wrapped(r.Entry.LastNotice)
		l.Importance = widget.WarningImportance
		objs = append(objs, subheading("Action needed"), l)
	}
	if a.Notes != "" {
		objs = append(objs, subheading("Notes"), wrapped(strings.TrimRight(a.Notes, "\n")))
	}
	if r.Final != nil {
		var buf bytes.Buffer
		(&cli.Renderer{Out: &buf, Verbose: 1}).Handle(*r.Final)
		objs = append(objs, subheading("Last run"), container.NewHScroll(widget.NewTextGridFromString(strings.TrimRight(buf.String(), "\n"))))
	}
	if raw, err := os.ReadFile(a.Path); err == nil {
		yaml := widget.NewTextGridFromString(strings.TrimRight(string(raw), "\n"))
		path := wrapped(a.Path)
		path.Importance = widget.LowImportance
		objs = append(objs, widget.NewAccordion(widget.NewAccordionItem("Definition", container.NewVBox(path, container.NewHScroll(yaml)))))
	}
	u.details.Objects = objs
	u.details.Refresh()
}

// setEnabled edits config.yaml like `updateapps enable/disable` and reloads.
func (u *ui) setEnabled(a *def.App, on bool) {
	ed, err := config.OpenEditor(u.cfg.Path)
	if err == nil {
		err = ed.SetApp(a.ID, a.Repo, on)
	}
	if err == nil {
		err = ed.Save()
	}
	if err != nil {
		dialog.NewError(err, u.win).Show()
		return
	}
	go u.reload(nil)
}

// installDir is the folder to open for an app, or "" when there isn't one.
func installDir(a *def.App) string {
	switch a.Install.Type {
	case def.InstallFile:
		return filepath.Dir(a.Install.Dest)
	case def.InstallExtract, def.InstallExtractOne:
		return a.Install.Dest
	}
	return ""
}

func (u *ui) openFolder(a *def.App) {
	dir := installDir(a)
	if _, err := os.Stat(dir); err != nil {
		dialog.NewInformation("Open folder", dir+" doesn't exist yet.", u.win).Show()
		return
	}
	u.openPath(dir)
}

// openPath hands a file or folder to the desktop's default handler.
func (u *ui) openPath(path string) {
	if err := u.app.OpenURL(&url.URL{Scheme: "file", Path: path}); err != nil {
		dialog.NewError(err, u.win).Show()
	}
}

// editDefinition opens the YAML in the desktop's editor. A definition from a
// fetched repository is replaced on the next pull, so it's copied to local/
// first, where it overrides the original.
func (u *ui) editDefinition(a *def.App) {
	var info *repo.Info
	for i := range u.set.Repos {
		if u.set.Repos[i].Name == a.Repo {
			info = &u.set.Repos[i]
		}
	}
	if info == nil || info.URL == "" {
		u.openPath(a.Path)
		return
	}
	var localDir string
	for _, r := range repo.Repositories(u.cfg) {
		if r.Name == repo.Local {
			localDir = repo.Dir(u.cfg, r)
		}
	}
	dst := filepath.Join(localDir, filepath.Base(a.Path))
	msg := "Repository " + a.Repo + " is replaced on every pull, so edits there are lost.\n\nCopy " + a.ID + " to " + dst + " and edit that? It then overrides " + a.Repo + "'s."
	dialog.NewConfirm("Edit definition", msg, func(ok bool) {
		if !ok {
			return
		}
		files := map[string]string{a.Path: dst}
		if a.Source.ScriptFile != "" && !filepath.IsAbs(a.Source.ScriptFile) {
			files[a.ScriptPath()] = filepath.Join(localDir, a.Source.ScriptFile)
		}
		for src, dst := range files {
			if err := copyFile(src, dst); err != nil {
				dialog.NewError(err, u.win).Show()
				return
			}
		}
		u.logf("copied %s/%s to %s; local now overrides it", a.Repo, filepath.Base(a.Path), dst)
		u.openPath(dst)
		go u.reload(nil)
	}, u.win).Show()
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil // already copied; don't clobber their edits
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
