package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
)

func enableCommands(f *flags) []*cobra.Command {
	var cmds []*cobra.Command
	for _, on := range []bool{true, false} {
		verb, state := "enable", "on"
		if !on {
			verb, state = "disable", "off"
		}
		var all bool
		cmd := &cobra.Command{
			Use:   verb + " ID... | --all [REPO...]",
			Short: "Turn apps " + state + " in config.yaml",
			Long: fmt.Sprintf(`Turn apps %[2]s by editing the enabled: and disabled: lists in config.yaml.
An app is named by its id, repo/id or name.

--all instead sets default: %[1]sd on every repository, or on the named ones, so
it covers definitions added later too. Apps listed by name under enabled: or
disabled: keep that setting, as do definitions that disable themselves.`, verb, state),
			RunE: func(cmd *cobra.Command, args []string) error { return setEnabled(f, on, all, args) },
		}
		cmd.Flags().BoolVar(&all, "all", false, "every app in all (or the named) repositories")
		cmds = append(cmds, cmd)
	}
	return cmds
}

func setEnabled(f *flags, on, all bool, args []string) error {
	cfg, apps, _, err := load(f)
	if err != nil {
		return err
	}
	switch {
	case cfg.DefsOverride != "":
		return &exitError{ExitConfig, "--defs bypasses config.yaml, so there's nothing to edit"}
	case !all && len(args) == 0:
		return &exitError{ExitConfig, "name the apps, or use --all"}
	}
	ed, err := config.OpenEditor(cfg.Path)
	if err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	word := map[bool]string{true: "enabled", false: "disabled"}

	var report []string
	if all {
		if report, err = setRepoDefaults(cfg, ed, word[on], args); err != nil {
			return &exitError{ExitConfig, err.Error()}
		}
	} else {
		var changed, already, blocked []string
		for _, arg := range args {
			a := findApp(apps, arg)
			if a == nil {
				return &exitError{ExitConfig, "no such app: " + arg + " (see `updateapps list`)"}
			}
			if a.IsEnabled() == on {
				already = append(already, a.ID)
				continue
			}
			if err := ed.SetApp(a.ID, a.Repo, on); err != nil {
				return &exitError{ExitConfig, err.Error()}
			}
			changed = append(changed, a.ID)
			if on && a.Blocked != "" {
				blocked = append(blocked, a.ID+": "+a.Blocked)
			}
		}
		if len(changed) > 0 {
			report = append(report, word[on]+": "+strings.Join(changed, ", "))
		}
		if len(already) > 0 {
			report = append(report, "already "+word[on]+": "+strings.Join(already, ", "))
		}
		for _, b := range blocked {
			report = append(report, "still won't run, needs trust: "+b)
		}
	}
	if err := ed.Save(); err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	for _, line := range report {
		fmt.Println(line)
	}
	return nil
}

// setRepoDefaults flips default: on the configured repositories (the named
// ones, or all) and reports the result, including what it doesn't override.
func setRepoDefaults(cfg *config.Config, ed *config.Editor, value string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var report []string
	for _, r := range cfg.Repositories {
		if len(names) > 0 && !want[r.Name] {
			continue
		}
		delete(want, r.Name)
		current := r.Default
		if current == "" {
			current = "enabled"
		}
		if current == value {
			report = append(report, r.Name+": already "+value+" by default")
			continue
		}
		if err := ed.SetRepoDefault(r.Name, value); err != nil {
			return nil, err
		}
		report = append(report, r.Name+": "+value+" by default")
	}
	for n := range want {
		return nil, fmt.Errorf("no repository named %s in %s", n, cfg.Path)
	}
	kept, keptWord := cfg.Disabled, "disabled"
	if value == "disabled" {
		kept, keptWord = cfg.Enabled, "enabled"
	}
	if len(kept) > 0 {
		report = append(report, fmt.Sprintf("still %s by name: %s", keptWord, strings.Join(kept, ", ")))
	}
	return report, nil
}

// findApp matches repo/id, id, then name.
func findApp(apps []*def.App, arg string) *def.App {
	for _, a := range apps {
		if arg == a.Repo+"/"+a.ID {
			return a
		}
	}
	if a, err := def.Find(apps, arg); err == nil {
		return a
	}
	return nil
}
