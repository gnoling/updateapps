package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/desktop"
	"github.com/gnoling/updateapps/internal/fetch"
)

// LauncherDirs is where launchers go, with icon URLs fetched through client.
func LauncherDirs(client *fetch.Client) (desktop.Dirs, error) {
	dirs, err := desktop.DefaultDirs()
	if err != nil {
		return dirs, err
	}
	dirs.Download = func(ctx context.Context, url, file string) error {
		return client.Download(ctx, url, file, fetch.DownloadOpts{})
	}
	return dirs, nil
}

// Launcher is the engine's after-install step, or nil when the config
// doesn't ask for desktop integration.
func Launcher(cfg *config.Config, client *fetch.Client) func(context.Context, *def.App) (string, error) {
	if !cfg.DesktopIntegration {
		return nil
	}
	return func(ctx context.Context, app *def.App) (string, error) {
		dirs, err := LauncherDirs(client)
		if err != nil {
			return "", err
		}
		res, err := dirs.Write(ctx, app)
		if err != nil {
			return "", err
		}
		line := res.String()
		if res.Outcome == desktop.Written && res.Note != "" {
			line += " (" + res.Note + ")"
		}
		return line, nil
	}
}

func desktopCommand(f *flags) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "desktop [FILTER...]",
		Short: "Write menu launchers for installed apps, or remove them",
		Long: `Write a launcher and icon into the applications menu for each matching app
that's installed, without updating anything. With desktop_integration: true
in the config, updates do this as they install.

An app you already have a launcher for is left to you, and only launchers
marked X-Updateapps-Managed=true are ever replaced or removed.

--remove deletes the launchers written for matching apps; with no filter,
every launcher this program wrote.`,
		RunE: func(cmd *cobra.Command, args []string) error { return desktopRun(cmd.Context(), f, args, remove) },
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the launchers instead")
	return cmd
}

func desktopRun(ctx context.Context, f *flags, filters []string, remove bool) error {
	cfg, apps, defErrs, err := load(f)
	if err != nil {
		return err
	}
	reportDefErrors(defErrs)
	dirs, err := LauncherDirs(fetch.New(fetch.GitHubToken(cfg.GitHubToken)))
	if err != nil {
		return err
	}
	dirs.DryRun = f.dryRun
	selected := def.Select(apps, filters)
	if len(selected) == 0 && len(filters) > 0 {
		return &exitError{ExitConfig, "no apps match " + filters[0]}
	}
	would := ""
	if f.dryRun {
		would = "dry run: "
	}

	if remove {
		ids := dirs.Managed()
		if len(filters) > 0 {
			ids = nil
			for _, a := range selected {
				ids = append(ids, a.ID)
			}
		}
		n := 0
		for _, id := range ids {
			ok, err := dirs.Remove(id)
			if err != nil {
				return err
			}
			if ok {
				n++
				fmt.Printf("%s%s: removed %s\n", would, id, dirs.Path(id))
			}
		}
		fmt.Printf("%sRemoved %d launchers from %s\n", would, n, dirs.Applications)
		return nil
	}

	var written, kept, failed int
	for _, a := range selected {
		res, err := dirs.Write(ctx, a)
		switch {
		case err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "%s: %v\n", a.ID, err)
		case res.Outcome == desktop.Written:
			written++
			fmt.Printf("%s%s: %s\n", would, a.ID, res)
			if res.Note != "" {
				fmt.Printf("  %s\n", res.Note)
			}
		case res.Outcome == desktop.Kept:
			kept++
			if cfg.Verbose > 0 {
				fmt.Printf("%s: %s\n", a.ID, res)
			}
		case cfg.Verbose > 0 || len(filters) > 0:
			fmt.Printf("%s: %s\n", a.ID, res)
		}
	}
	fmt.Printf("%sWrote %d launchers to %s; %d apps have one of yours\n", would, written, dirs.Applications, kept)
	if failed > 0 {
		return &exitError{code: ExitAppFailed}
	}
	return nil
}
