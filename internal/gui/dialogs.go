package gui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/repo"
)

// editConfig applies fn through the config editor, saves, and reloads.
func (u *ui) editConfig(fn func(*config.Editor) error, then func()) {
	if u.cfg.DefsOverride != "" {
		dialog.NewInformation("Read-only", "--defs bypasses config.yaml, so there's nothing to edit.", u.win).Show()
		return
	}
	ed, err := config.OpenEditor(u.cfg.Path)
	if err == nil {
		err = fn(ed)
	}
	if err == nil {
		err = ed.Save()
	}
	if err != nil {
		dialog.NewError(err, u.win).Show()
		return
	}
	go u.reload(then)
}

func (u *ui) settingsDialog() {
	if u.cfg == nil {
		return
	}
	c := u.cfg
	appdir := widget.NewEntry()
	appdir.SetText(c.AppDir)
	appimagedir := widget.NewEntry()
	appimagedir.SetText(c.AppImageDir)
	jobs := widget.NewEntry()
	jobs.SetText(strconv.Itoa(c.Jobs))
	privilege := widget.NewSelect([]string{"auto", "sudo", "pkexec", "none"}, nil)
	privilege.SetSelected(c.Privilege)
	scope := widget.NewSelect([]string{"user", "system"}, nil)
	scope.SetSelected(c.FlatpakScope)
	token := widget.NewPasswordEntry()
	token.SetText(c.GitHubToken)
	token.SetPlaceHolder("falls back to $GITHUB_TOKEN, then gh")
	autoPull := widget.NewCheck("Refresh fetched repositories before every update", nil)
	autoPull.SetChecked(c.AutoPull)
	every := widget.NewEntry()
	if c.CheckEvery > 0 {
		every.SetText(c.CheckEvery.String())
	}
	every.SetPlaceHolder("e.g. 6h or 30m; empty = never")

	items := []*widget.FormItem{
		widget.NewFormItem("Apps folder", appdir),
		widget.NewFormItem("AppImages folder", appimagedir),
		widget.NewFormItem("Parallel jobs", jobs),
		widget.NewFormItem("Root for .deb installs", privilege),
		widget.NewFormItem("Flatpak scope", scope),
		widget.NewFormItem("GitHub token", token),
		widget.NewFormItem("", autoPull),
		widget.NewFormItem("Check for updates every", every),
	}
	d := dialog.NewForm("Settings", "Save", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(jobs.Text))
		if err != nil || n < 1 {
			dialog.NewError(errors.New("parallel jobs must be a number of at least 1"), u.win).Show()
			return
		}
		var interval time.Duration
		if t := strings.TrimSpace(every.Text); t != "" {
			if interval, err = time.ParseDuration(t); err != nil || interval < time.Minute {
				dialog.NewError(errors.New("the check interval is a duration of at least 1m, like 6h or 90m"), u.win).Show()
				return
			}
		}
		u.editConfig(func(ed *config.Editor) error {
			// Only what changed is written, so a default stays a default.
			changes := []struct {
				key      string
				old, new any
			}{
				{"appdir", c.AppDir, strings.TrimSpace(appdir.Text)},
				{"appimagedir", c.AppImageDir, strings.TrimSpace(appimagedir.Text)},
				{"jobs", c.Jobs, n},
				{"privilege", c.Privilege, privilege.Selected},
				{"flatpak_scope", c.FlatpakScope, scope.Selected},
				{"auto_pull", c.AutoPull, autoPull.Checked},
			}
			for _, ch := range changes {
				if ch.old != ch.new {
					if err := ed.SetScalar(ch.key, ch.new); err != nil {
						return err
					}
				}
			}
			if t := strings.TrimSpace(token.Text); t != c.GitHubToken {
				var v any = t
				if t == "" {
					v = nil
				}
				if err := ed.SetScalar("github_token", v); err != nil {
					return err
				}
			}
			if interval != c.CheckEvery {
				var v any = interval.String()
				if interval == 0 {
					v = nil
				}
				return ed.SetScalar("check_interval", v)
			}
			return nil
		}, nil)
	}, u.win)
	d.Resize(fyne.NewSize(620, 0))
	d.Show()
}

const trustWarning = "A trusted repository's definitions may run shell commands, install system packages as root and write anywhere. Trust only what you'd let run a script as you."

func (u *ui) reposDialog() {
	if u.cfg == nil || u.set == nil {
		return
	}
	if u.cfg.DefsOverride != "" {
		dialog.NewInformation("Repositories", "--defs is in effect: one flat trusted folder, "+u.cfg.DefsOverride, u.win).Show()
		return
	}
	configured := map[string]bool{}
	for _, r := range u.cfg.Repositories {
		configured[r.Name] = true
	}
	var d dialog.Dialog
	reopen := func() { d.Hide(); u.reposDialog() }

	box := container.NewVBox()
	for _, info := range u.set.Repos {
		info := info
		var lines []string
		switch {
		case info.URL != "":
			lines = append(lines, info.URL+" (fetched; don't edit its folder)")
		case info.Name == repo.Local && !configured[info.Name]:
			lines = append(lines, info.Dir+" (yours; read last, so it overrides the others)")
		default:
			lines = append(lines, info.Dir+" (yours)")
		}
		switch {
		case info.Problem != "":
			lines = append(lines, "Problem: "+info.Problem)
		case !info.Meta.PulledAt.IsZero():
			lines = append(lines, fmt.Sprintf("%d definitions, fetched %s%s", info.Apps, info.Meta.PulledAt.Local().Format("2006-01-02 15:04"), at(info.Meta.Commit)))
		default:
			lines = append(lines, fmt.Sprintf("%d definitions", info.Apps))
		}
		desc := wrapped(strings.Join(lines, "\n"))
		desc.Importance = widget.LowImportance

		trusted := widget.NewCheck("Trusted", nil)
		trusted.Checked = info.Trusted
		trusted.OnChanged = func(on bool) {
			if !on {
				u.setRepoField(info, "trusted", nil, reopen)
				return
			}
			dialog.NewConfirm("Trust "+info.Name+"?", trustWarning, func(ok bool) {
				if ok {
					u.setRepoField(info, "trusted", true, reopen)
				} else {
					trusted.SetChecked(false)
				}
			}, u.win).Show()
		}
		def := widget.NewSelect([]string{"enabled", "disabled"}, nil)
		if info.Default == "" {
			def.SetSelected("enabled")
		} else {
			def.SetSelected(info.Default)
		}
		def.OnChanged = func(v string) {
			if v != info.Default && !(v == "enabled" && info.Default == "") {
				u.setRepoField(info, "default", v, reopen)
			}
		}
		controls := container.NewHBox(trusted, widget.NewLabel("Apps by default:"), def)
		if info.URL != "" {
			controls.Add(widget.NewButton("Pull", func() {
				d.Hide()
				go u.pull([]string{info.Name}, u.reposDialog)
			}))
		}
		if configured[info.Name] {
			controls.Add(widget.NewButton("Remove", func() { u.removeRepo(info, reopen) }))
		}
		title := info.Name
		if info.Manifest.Name != "" && info.Manifest.Name != info.Name {
			title += " — " + info.Manifest.Name
		}
		content := []fyne.CanvasObject{desc, controls}
		if info.Manifest.Description != "" {
			content = append([]fyne.CanvasObject{wrapped(info.Manifest.Description)}, content...)
		}
		box.Add(widget.NewCard(title, "", container.NewVBox(content...)))
	}
	box.Add(widget.NewButton("Add repository…", func() {
		d.Hide()
		u.addRepoDialog()
	}))
	d = dialog.NewCustom("Repositories", "Close", container.NewScroll(box), u.win)
	d.Resize(fyne.NewSize(760, 560))
	d.Show()
}

// setRepoField edits one repository key. The implicit local/ isn't in the
// file yet, so it's written out first.
func (u *ui) setRepoField(info repo.Info, key string, value any, then func()) {
	u.editConfig(func(ed *config.Editor) error {
		for _, r := range u.cfg.Repositories {
			if r.Name == info.Name {
				return ed.SetRepoField(info.Name, key, value)
			}
		}
		r := config.Repository{Name: info.Name}
		switch key {
		case "trusted":
			r.Trusted, _ = value.(bool)
		case "default":
			r.Default, _ = value.(string)
		}
		return ed.AddRepository(r)
	}, then)
}

func (u *ui) removeRepo(info repo.Info, then func()) {
	fetched := false
	if _, err := os.Stat(filepath.Join(info.Dir, ".updateapps-fetched.json")); err == nil {
		fetched = true
	}
	msg := "Remove " + info.Name + " from " + u.cfg.Path + "?"
	if fetched {
		msg += "\n\nIts fetched copy in " + info.Dir + " is deleted too."
	} else if info.Path == "" {
		msg += "\n\n" + info.Dir + " is left alone; move or delete it yourself."
	}
	dialog.NewConfirm("Remove repository", msg, func(ok bool) {
		if !ok {
			return
		}
		u.editConfig(func(ed *config.Editor) error { return ed.RemoveRepository(info.Name) }, func() {
			if fetched {
				if err := os.RemoveAll(info.Dir); err != nil {
					dialog.NewError(err, u.win).Show()
				}
				go u.reload(then)
				return
			}
			then()
		})
	}, u.win).Show()
}

func (u *ui) addRepoDialog() {
	name := widget.NewEntry()
	name.SetPlaceHolder("a short word; names apps.d/<name>/")
	url := widget.NewEntry()
	url.SetPlaceHolder("https://github.com/owner/repo, GitLab, Forgejo or a .tar.gz")
	path := widget.NewEntry()
	path.SetPlaceHolder("a folder of your own instead of a url")
	branch := widget.NewEntry()
	branch.SetPlaceHolder("the host's default")
	def := widget.NewSelect([]string{"enabled", "disabled"}, nil)
	def.SetSelected("disabled")
	trusted := widget.NewCheck("Trusted", nil)
	warn := wrapped(trustWarning)
	warn.Importance = widget.LowImportance
	items := []*widget.FormItem{
		widget.NewFormItem("Name", name),
		widget.NewFormItem("URL", url),
		widget.NewFormItem("or path", path),
		widget.NewFormItem("Branch", branch),
		widget.NewFormItem("Apps by default", def),
		widget.NewFormItem("", container.NewVBox(trusted, warn)),
	}
	d := dialog.NewForm("Add repository", "Add", "Cancel", items, func(ok bool) {
		if !ok {
			u.reposDialog()
			return
		}
		r := config.Repository{
			Name: strings.TrimSpace(name.Text), URL: strings.TrimSpace(url.Text), Path: strings.TrimSpace(path.Text),
			Branch: strings.TrimSpace(branch.Text), Trusted: trusted.Checked,
		}
		if def.Selected == "disabled" {
			r.Default = "disabled"
		}
		if r.Name == "" {
			dialog.NewError(errors.New("a repository needs a name"), u.win).Show()
			return
		}
		// Save validates the rest (name shape, url xor path).
		u.editConfig(func(ed *config.Editor) error { return ed.AddRepository(r) }, u.reposDialog)
	}, u.win)
	d.Resize(fyne.NewSize(640, 0))
	d.Show()
}
