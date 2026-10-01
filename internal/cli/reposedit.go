package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/repo"
)

const trustWarning = "A trusted repository's definitions may run shell commands, install system packages as root and write anywhere. Trust only what you'd let run a script as you."

// confirmIn is where a yes/no answer is read from; tests replace it.
var confirmIn io.Reader = os.Stdin

// confirmTrust asks before trust is granted. Off a terminal there's nobody
// to ask, so it takes --yes.
func confirmTrust(name string, yes bool) error {
	if yes {
		return nil
	}
	if confirmIn == io.Reader(os.Stdin) && !isTerminal(os.Stdin) {
		return &exitError{ExitConfig, "trusting " + name + " needs a yes: run this at a terminal, or pass --yes"}
	}
	fmt.Printf("%s\nTrust %s? [y/N] ", trustWarning, name)
	answer, _ := bufio.NewReader(confirmIn).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	}
	return &exitError{ExitConfig, "not trusted"}
}

// editRepos applies fn to config.yaml through the editor, then checks the
// result still loads.
func editRepos(f *flags, fn func(cfg *config.Config, ed *config.Editor) error) error {
	cfg, err := loadConfig(f)
	if err != nil {
		return err
	}
	if cfg.DefsOverride != "" {
		return &exitError{ExitConfig, "--defs bypasses config.yaml, so there's nothing to edit"}
	}
	ed, err := config.OpenEditor(cfg.Path)
	if err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	if err := fn(cfg, ed); err != nil {
		if _, ok := err.(*exitError); ok {
			return err
		}
		return &exitError{ExitConfig, err.Error()}
	}
	if err := ed.Save(); err != nil {
		return &exitError{ExitConfig, err.Error()}
	}
	return nil
}

func configured(cfg *config.Config, name string) bool {
	for _, r := range cfg.Repositories {
		if r.Name == name {
			return true
		}
	}
	return false
}

// setRepoField edits one key of a repository. The implicit local/ isn't in
// the file, so it's written out first.
func setRepoField(cfg *config.Config, ed *config.Editor, name, key string, value any) error {
	if configured(cfg, name) {
		return ed.SetRepoField(name, key, value)
	}
	if name != repo.Local {
		return fmt.Errorf("no repository named %s in %s", name, cfg.Path)
	}
	r := config.Repository{Name: name}
	r.Trusted, _ = value.(bool)
	return ed.AddRepository(r)
}

func reposEditCommands(f *flags) []*cobra.Command {
	var add config.Repository
	var yes bool
	addCmd := &cobra.Command{
		Use:   "add NAME (--url URL | --path DIR)",
		Short: "Add a definition repository to config.yaml",
		Long: `Add a definition repository to config.yaml, after the ones already there (a
later repository wins on the same id).

--url names a GitHub, GitLab or Forgejo repository, or a .tar.gz/.zip link;
it's fetched into apps.d/NAME/ now. --path names a folder of your own
instead. Its apps start disabled unless --default enabled says otherwise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			add.Name = args[0]
			if add.Trusted {
				if err := confirmTrust(add.Name, yes); err != nil {
					return err
				}
			}
			err := editRepos(f, func(cfg *config.Config, ed *config.Editor) error {
				if configured(cfg, add.Name) {
					return fmt.Errorf("%s already has a repository named %s", cfg.Path, add.Name)
				}
				if add.Default == "enabled" {
					add.Default = ""
				}
				return ed.AddRepository(add)
			})
			if err != nil {
				return err
			}
			fmt.Printf("added %s\n", add.Name)
			if add.URL == "" {
				return nil
			}
			cfg, err := loadConfig(f)
			if err != nil {
				return err
			}
			if pullRepos(cmd.Context(), cfg, []string{add.Name}, false) > 0 {
				return &exitError{code: ExitAppFailed}
			}
			return nil
		},
	}
	fl := addCmd.Flags()
	fl.StringVar(&add.URL, "url", "", "where to fetch it from")
	fl.StringVar(&add.Path, "path", "", "a folder of your own, instead of a url")
	fl.StringVar(&add.Branch, "branch", "", "branch to fetch (default: the host's)")
	fl.StringVar(&add.Default, "default", "disabled", "whether its apps start enabled or disabled")
	fl.BoolVar(&add.Trusted, "trusted", false, "trust it (asks first)")
	fl.BoolVar(&yes, "yes", false, "don't ask before trusting")

	removeCmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a repository from config.yaml",
		Long: `Remove a repository from config.yaml. A fetched copy in apps.d/NAME/ is
deleted with it; a folder of your own is left alone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, dir := args[0], ""
			err := editRepos(f, func(cfg *config.Config, ed *config.Editor) error {
				for _, r := range cfg.Repositories {
					if r.Name == name {
						dir = repo.Dir(cfg, r)
					}
				}
				if dir == "" {
					return fmt.Errorf("no repository named %s in %s", name, cfg.Path)
				}
				return ed.RemoveRepository(name)
			})
			if err != nil {
				return err
			}
			fmt.Printf("removed %s\n", name)
			if _, err := os.Stat(filepath.Join(dir, ".updateapps-fetched.json")); err == nil {
				if err := os.RemoveAll(dir); err != nil {
					return err
				}
				fmt.Printf("deleted its fetched copy, %s\n", dir)
			} else if _, err := os.Stat(dir); err == nil {
				fmt.Printf("%s is yours and was left alone\n", dir)
			}
			return nil
		},
	}

	var trustYes bool
	trustCmd := &cobra.Command{
		Use:   "trust NAME",
		Short: "Let a repository's definitions run shell commands and install as root",
		Long:  trustWarning + "\n\nAsks first; --yes answers for you.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			err := editRepos(f, func(cfg *config.Config, ed *config.Editor) error {
				if !configured(cfg, name) && name != repo.Local {
					return fmt.Errorf("no repository named %s in %s", name, cfg.Path)
				}
				if err := confirmTrust(name, trustYes); err != nil {
					return err
				}
				return setRepoField(cfg, ed, name, "trusted", true)
			})
			if err == nil {
				fmt.Printf("%s is trusted\n", name)
			}
			return err
		},
	}
	trustCmd.Flags().BoolVar(&trustYes, "yes", false, "don't ask")

	untrustCmd := &cobra.Command{
		Use:   "untrust NAME",
		Short: "Take a repository's trust away",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			err := editRepos(f, func(cfg *config.Config, ed *config.Editor) error {
				if !configured(cfg, name) {
					if name == repo.Local {
						return nil // never written out, so never trusted
					}
					return fmt.Errorf("no repository named %s in %s", name, cfg.Path)
				}
				return ed.SetRepoField(name, "trusted", nil)
			})
			if err == nil {
				fmt.Printf("%s is not trusted\n", name)
			}
			return err
		},
	}
	return []*cobra.Command{addCmd, removeCmd, trustCmd, untrustCmd}
}
