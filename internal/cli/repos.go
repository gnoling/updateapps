package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/repo"
)

func reposCommand(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use: "repos", Short: "List the definition repositories", Args: cobra.NoArgs,
		Long: `List the definition repositories from config.yaml.

Each one's definitions live in apps.d/<name>/ beside config.yaml, read in
order; a later one wins on the same id. With a url: it's fetched by 'repos
pull' and replaced each time, so don't edit it. apps.d/local/ is yours and
read last: copy a definition there to change it.

Only a repository marked trusted: true may run shell commands, install as
root or write outside the apps directories.`,
		RunE: func(cmd *cobra.Command, args []string) error { return listRepos(f) },
	}
	cmd.AddCommand(&cobra.Command{
		Use: "pull [NAME...]", Short: "Fetch the latest definitions for url: repositories",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(f)
			if err != nil {
				return err
			}
			if failed := pullRepos(cmd.Context(), cfg, args, false); failed > 0 {
				return &exitError{code: ExitAppFailed}
			}
			return nil
		},
	})
	return cmd
}

func listRepos(f *flags) error {
	cfg, err := loadConfig(f)
	if err != nil {
		return err
	}
	lastSet = repo.Load(cfg) // not load(): an unfetched repository is exactly what this should show
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDEFINITIONS\tTRUSTED\tDEFAULT\tFETCHED\tSOURCE")
	for _, r := range lastSet.Repos {
		trusted, def, fetched := "no", "enabled", "-"
		if r.Trusted {
			trusted = "yes"
		}
		if r.Default != "" {
			def = r.Default
		}
		switch {
		case r.Problem != "":
			fetched = "! " + r.Problem
		case r.URL == "":
			fetched = "(yours)"
		case !r.Meta.PulledAt.IsZero():
			fetched = r.Meta.PulledAt.Local().Format("2006-01-02 15:04")
			if len(r.Meta.Commit) >= 7 {
				fetched += " @" + r.Meta.Commit[:7]
			}
		}
		source := r.URL
		if source == "" {
			source = r.Dir
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", r.Name, r.Apps, trusted, def, fetched, source)
	}
	return w.Flush()
}

// fetchMissing does a url: repository's first fetch when something needs its
// definitions. Refreshing one that's already here is `repos pull` or auto_pull.
func fetchMissing(cfg *config.Config) {
	var names []string
	for _, r := range repo.Repositories(cfg) {
		if _, err := os.Stat(repo.Dir(cfg, r)); r.URL != "" && os.IsNotExist(err) {
			names = append(names, r.Name)
		}
	}
	if len(names) > 0 {
		pullRepos(context.Background(), cfg, names, false)
	}
}

// pullRepos fetches the named url: repositories (all if none are named) and
// returns the failure count. quiet prints only changes.
func pullRepos(ctx context.Context, cfg *config.Config, names []string, quiet bool) (failed int) {
	client := fetch.New(fetch.GitHubToken(cfg.GitHubToken))
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	found := map[string]bool{}
	for _, r := range repo.Repositories(cfg) {
		if len(names) > 0 && !want[r.Name] {
			continue
		}
		found[r.Name] = true
		if r.URL == "" {
			if len(names) > 0 {
				fmt.Printf("%s: no url:, so %s is yours to manage\n", r.Name, repo.Dir(cfg, r))
			}
			continue
		}
		change, err := repo.Pull(ctx, client, cfg, r)
		switch {
		case err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "updateapps: repository %s: %v\n", r.Name, err)
		case change.UpToDate || len(change.Added)+len(change.Changed)+len(change.Removed) == 0:
			if !quiet {
				fmt.Printf("%s: up to date%s\n", r.Name, at(change.Commit))
			}
		default:
			fmt.Printf("%s: updated%s\n", r.Name, at(change.Commit))
			for _, part := range []struct {
				label string
				ids   []string
			}{{"new", change.Added}, {"changed", change.Changed}, {"removed", change.Removed}} {
				switch n := len(part.ids); {
				case n > 12: // a first fetch, typically
					fmt.Printf("  %-8s %d definitions\n", part.label, n)
				case n > 0:
					fmt.Printf("  %-8s %s\n", part.label, strings.Join(part.ids, ", "))
				}
			}
			if len(change.NowNeedTrust) > 0 && !r.Trusted {
				fmt.Printf("  these now need trust and won't run unless you grant it: %s\n", strings.Join(change.NowNeedTrust, ", "))
			}
		}
	}
	for _, n := range names {
		if !found[n] {
			failed++
			fmt.Fprintf(os.Stderr, "updateapps: no repository named %s\n", n)
		}
	}
	return failed
}

func at(commit string) string {
	if len(commit) < 7 {
		return ""
	}
	return " (" + commit[:7] + ")"
}
