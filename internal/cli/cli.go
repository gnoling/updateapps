package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/engine"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/install"
	"github.com/gnoling/updateapps/internal/repo"
	"github.com/gnoling/updateapps/internal/state"
)

// Exit codes.
const (
	ExitOK        = 0
	ExitAppFailed = 1 // at least one app failed
	ExitConfig    = 2 // config or definition errors
)

// exitError carries an exit code up to Main; an empty message prints nothing.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

type flags struct {
	config, defs string
	jobs         int
	force        bool
	verbose      int
	dryRun       bool
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	root := newRoot()
	root.SetArgs(args)
	err := root.Execute()
	var ee *exitError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "updateapps:", ee.msg)
		}
		return ee.code
	default:
		fmt.Fprintln(os.Stderr, "updateapps:", err)
		return ExitConfig
	}
}

func newRoot() *cobra.Command {
	f := &flags{}
	root := &cobra.Command{
		Use:   "updateapps [FILTER...]",
		Short: "Keep locally installed apps up to date from their upstream releases",
		Long: `Keep locally installed apps up to date from their upstream releases.

With no command, updates every enabled app, or those matching a FILTER: a
case-insensitive substring of an app's id, name or repo/URL. Filters are OR'd.
Naming a disabled app exactly runs it anyway.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return run(cmd.Context(), f, engine.ModeUpdate, args) },
	}
	pf := root.PersistentFlags()
	pf.StringVar(&f.config, "config", "", "config file (default "+config.DefaultPath()+")")
	pf.StringVar(&f.defs, "defs", "", "use just this flat folder of definitions, trusted (for developing a repository)")
	pf.IntVarP(&f.jobs, "jobs", "j", 0, "apps to process at once (default 12)")
	pf.BoolVarP(&f.force, "force", "f", false, "ignore recorded versions and reinstall")
	pf.CountVarP(&f.verbose, "verbose", "v", "show detail per app (-vv adds HTTP requests)")
	pf.BoolVar(&f.dryRun, "dry-run", false, "resolve versions but download and record nothing")

	root.AddCommand(
		&cobra.Command{
			Use: "update [FILTER...]", Short: "Update matching apps (the default command)",
			RunE: func(cmd *cobra.Command, args []string) error { return run(cmd.Context(), f, engine.ModeUpdate, args) },
		},
		&cobra.Command{
			Use: "check [FILTER...]", Short: "Report what has a new version, downloading nothing",
			RunE: func(cmd *cobra.Command, args []string) error { return run(cmd.Context(), f, engine.ModeCheck, args) },
		},
		&cobra.Command{
			Use: "mark-current [FILTER...]", Short: "Record the latest versions of already-installed apps without downloading",
			Long: `Record the latest version of matching apps that are already installed,
downloading nothing. For adopting an existing setup, so the first update
doesn't re-download everything. Apps that aren't installed are left alone.`,
			RunE: func(cmd *cobra.Command, args []string) error {
				return run(cmd.Context(), f, engine.ModeMarkCurrent, args)
			},
		},
		&cobra.Command{
			Use: "list [FILTER...]", Short: "List apps with their recorded version and last status",
			RunE: func(cmd *cobra.Command, args []string) error { return list(f, args) },
		},
		&cobra.Command{
			Use: "show ID", Short: "Show one app's definition and state", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return show(f, args[0]) },
		},
		reposCommand(f),
	)
	root.AddCommand(enableCommands(f)...)
	root.AddCommand(
		&cobra.Command{
			Use: "validate", Short: "Load and validate every definition", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error { return validate(f) },
		},
	)
	return root
}

// lastSet is the most recent load, for commands that want repository detail.
var lastSet *repo.Set

// load reads the config and every repository's definitions.
func load(f *flags) (*config.Config, []*def.App, []error, error) {
	cfg, err := loadConfig(f)
	if err != nil {
		return nil, nil, nil, err
	}
	fetchMissing(cfg)
	set := repo.Load(cfg)
	lastSet = set
	if len(set.Apps) == 0 && len(set.Repos) == 1 && set.Repos[0].Problem != "" {
		return nil, nil, nil, &exitError{ExitConfig, fmt.Sprintf(
			"definitions: %s: %s (set `repositories:` in %s, or pass --defs)",
			set.Repos[0].Name, set.Repos[0].Problem, cfg.Path)}
	}
	return cfg, set.Apps, set.Errors, nil
}

func loadConfig(f *flags) (*config.Config, error) {
	cfg, err := config.Load(f.config)
	if err != nil {
		return nil, &exitError{ExitConfig, err.Error()}
	}
	if f.defs != "" {
		// One explicit flat folder replaces the configured repositories.
		cfg.DefsOverride = f.defs
	}
	if f.jobs > 0 {
		cfg.Jobs = f.jobs
	}
	if f.force {
		cfg.Force = true
	}
	if f.verbose > 0 {
		cfg.Verbose = f.verbose
	}
	return cfg, nil
}

// definitionsFrom names where definitions come from, for messages.
func definitionsFrom(cfg *config.Config) string {
	if cfg.DefsOverride != "" {
		return cfg.DefsOverride
	}
	var names []string
	for _, r := range lastSet.Repos {
		if r.Apps > 0 || r.Name != repo.Local {
			names = append(names, r.Name)
		}
	}
	if len(names) == 1 {
		return "repository " + names[0]
	}
	return fmt.Sprintf("%d repositories (%s)", len(names), strings.Join(names, ", "))
}

func reportDefErrors(errs []error) {
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "updateapps:", e)
	}
}

func run(ctx context.Context, f *flags, mode engine.Mode, filters []string) error {
	if cfg, err := loadConfig(f); err == nil && cfg.AutoPull && mode == engine.ModeUpdate && !f.dryRun {
		// Best effort: a failed pull mustn't block the update.
		pullRepos(ctx, cfg, nil, true)
	}
	cfg, apps, defErrs, err := load(f)
	if err != nil {
		return err
	}
	// A bad definition doesn't stop the run; it only makes the exit code
	// nonzero.
	reportDefErrors(defErrs)

	selected := def.Select(apps, filters)
	if len(selected) == 0 {
		if len(filters) > 0 {
			return &exitError{ExitConfig, "no apps match " + strings.Join(filters, ", ")}
		}
		return &exitError{ExitConfig, "nothing is enabled in " + definitionsFrom(cfg) + "; see `updateapps list`, then name apps under enabled: in " + cfg.Path}
	}

	st, err := state.Open(cfg.State)
	if err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	eng := &engine.Engine{
		Client:    fetch.New(fetch.GitHubToken(cfg.GitHubToken)),
		State:     st,
		StatePath: cfg.State,
		Vars:      cfg.Vars(),
		System: install.System{
			Privilege:   cfg.Privilege,
			Interactive: isTerminal(os.Stdin) && isTerminal(os.Stderr),
			PendingDir:  config.PendingDir(),
			FlatpakUser: cfg.FlatpakScope == "user",
		},
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	events, err := eng.Run(ctx, selected, engine.Options{Mode: mode, Jobs: cfg.Jobs, Force: cfg.Force, DryRun: f.dryRun})
	if err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	r := &Renderer{Out: os.Stdout, Verbose: cfg.Verbose, Color: useColor(os.Stdout), Mode: mode, DryRun: f.dryRun}
	sum := r.Drain(events)

	switch {
	case len(defErrs) > 0:
		return &exitError{code: ExitConfig}
	case sum.Failed > 0 || sum.Cancelled:
		return &exitError{code: ExitAppFailed}
	}
	return nil
}

func validate(f *flags) error {
	cfg, apps, defErrs, err := load(f)
	if err != nil {
		return err
	}
	reportDefErrors(defErrs)
	fmt.Printf("%d valid, %d invalid in %s\n", len(apps), len(defErrs), definitionsFrom(cfg))
	blocked := 0
	for _, a := range apps {
		if a.Blocked != "" {
			blocked++
			fmt.Printf("needs trust: %s: %s\n", a.ID, a.Blocked)
		}
	}
	for _, line := range lastSet.Shadowed {
		fmt.Println("override:", line)
	}
	if len(defErrs) > 0 {
		return &exitError{code: ExitConfig}
	}
	return nil
}

func list(f *flags, filters []string) error {
	cfg, apps, defErrs, err := load(f)
	if err != nil {
		return err
	}
	reportDefErrors(defErrs)
	st, err := state.Open(cfg.State)
	if err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	if len(filters) == 0 {
		filters = []string{""} // list shows disabled apps too
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	multi := cfg.DefsOverride == ""
	header := "ID\tCATEGORY\tVERSION\tINSTALLED\tSTATUS"
	if multi {
		header = "ID\tREPO\tCATEGORY\tVERSION\tINSTALLED\tSTATUS"
	}
	fmt.Fprintln(w, header)
	for _, a := range apps {
		if !matchesAny(a, filters) {
			continue
		}
		e := st.Get(a.ID)
		version, installed, status := e.Display, "-", "ok"
		if version == "" {
			version = "-"
		}
		if e.InstalledAt != nil {
			installed = e.InstalledAt.Local().Format("2006-01-02 15:04")
		}
		switch {
		case a.Blocked != "":
			status = "needs trust"
		case !a.IsEnabled():
			status = "disabled"
		case e.LastError != "":
			status = "error: " + firstLine(e.LastError)
		case e.LastNotice != "":
			status = "action needed"
		case e.Version == "":
			status = "never installed"
		}
		if multi {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Repo, a.Category, version, installed, status)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Category, version, installed, status)
		}
	}
	return w.Flush()
}

// matchesAny is Select's substring rule without the enabled check.
func matchesAny(a *def.App, filters []string) bool {
	enabled := true
	probe := *a
	probe.Enabled, probe.Blocked = &enabled, "" // list shows disabled and blocked apps too
	return len(def.Select([]*def.App{&probe}, filters)) == 1
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 70 {
		line = line[:67] + "..."
	}
	return line
}

func show(f *flags, id string) error {
	cfg, apps, defErrs, err := load(f)
	if err != nil {
		return err
	}
	app, err := def.Find(apps, id)
	if err != nil {
		reportDefErrors(defErrs) // the one they asked for may be among the broken
		return &exitError{ExitConfig, err.Error()}
	}
	raw, err := os.ReadFile(app.Path)
	if err != nil {
		return err
	}
	st, err := state.Open(cfg.State)
	if err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	entry, _ := json.MarshalIndent(st.Get(app.ID), "", "  ")
	fmt.Printf("# %s (repository: %s)\n%s\n# installs to: %s\n", app.Path, app.Repo, strings.TrimRight(string(raw), "\n"), installTarget(app))
	if app.Blocked != "" {
		fmt.Printf("# NOT RUN: %s\n", app.Blocked)
	}
	fmt.Printf("# state (%s):\n%s\n", cfg.State, entry)
	return nil
}

func installTarget(a *def.App) string {
	switch a.Install.Type {
	case def.InstallNone:
		return "(nothing; download is handed to post hooks)"
	case def.InstallDeb:
		return "(system package via apt; needs root)"
	case def.InstallFlatpak:
		return "(flatpak)"
	}
	return a.Install.Dest
}
