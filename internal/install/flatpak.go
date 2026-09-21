package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/gnoling/updateapps/internal/flatpak"
)

// InstallFlatpaks handles items one at a time, by what each is:
//
//	.flatpak bundle -> install --reinstall --bundle
//	.flatpakref     -> install --from the first time, update after
//	no file         -> install or update source.app from source.remote
//
// flatpak elevates through polkit itself, so nothing is wrapped in sudo.
func (s System) InstallFlatpaks(ctx context.Context, items []Item, logf func(item int) func(string, ...any)) []Outcome {
	out := make([]Outcome, len(items))
	if err := flatpak.Available(); err != nil {
		for i := range out {
			out[i].Err = err
		}
		return out
	}
	for i, it := range items {
		if ctx.Err() != nil {
			out[i].Err = ctx.Err()
			continue
		}
		out[i] = s.installFlatpak(ctx, it, logf(i))
	}
	return out
}

func (s System) installFlatpak(ctx context.Context, it Item, logf func(string, ...any)) Outcome {
	user := s.FlatpakUser
	if it.App.Install.Scope != "" {
		user = it.App.Install.Scope == "user"
	}
	scope := flatpak.ScopeFlag(user)
	base := []string{scope, "-y", "--noninteractive"}
	flatpakCmd := func(args ...string) error {
		return runLogged(exec.CommandContext(ctx, "flatpak", args...), logf)
	}

	src := it.App.Source
	ref := flatpak.Ref{ID: src.App, Branch: src.Branch, Remote: src.Remote}
	if ref.Branch == "" {
		ref.Branch = "stable"
	}
	var err error
	switch {
	case it.File == "": // an app on a remote that's already configured
		if _, _, installed := flatpak.Installed(ctx, user, ref); installed {
			err = flatpakCmd(append([]string{"update"}, append(base, ref.String())...)...)
		} else {
			err = flatpakCmd(append([]string{"install"}, append(base, ref.Remote, ref.String())...)...)
		}
	default:
		data, rerr := os.ReadFile(it.File)
		if rerr != nil {
			return Outcome{Err: rerr}
		}
		if !flatpak.IsRef(data) {
			// A bundle's version is its release; its app id isn't known here.
			err = flatpakCmd(append([]string{"install"}, append(base, "--reinstall", "--bundle", it.File)...)...)
			return s.flatpakOutcome(err, it, fmt.Sprintf("flatpak install %s --bundle %s", scope, it.File))
		}
		if ref, rerr = flatpak.ParseRef(data); rerr != nil {
			return Outcome{Err: rerr}
		}
		if _, _, installed := flatpak.Installed(ctx, user, ref); installed {
			err = flatpakCmd(append([]string{"update"}, append(base, ref.String())...)...)
		} else {
			err = flatpakCmd(append([]string{"install"}, append(base, "--from", it.File)...)...)
		}
	}
	o := s.flatpakOutcome(err, it, fmt.Sprintf("flatpak update %s %s", scope, ref))
	if o.Err != nil || o.ActionNeeded != "" {
		return o
	}
	// flatpak is the authority on what's installed now.
	commit, origin, installed := flatpak.Installed(ctx, user, ref)
	if !installed {
		return Outcome{Err: fmt.Errorf("flatpak reported success but %s isn't installed", ref)}
	}
	o.Version, o.Display = commit, flatpak.Display("", commit)
	if remote, rerr := flatpak.RemoteInfo(ctx, user, origin, ref); rerr == nil && remote.Commit == commit {
		o.Display = flatpak.Display(remote.Version, commit)
	}
	return o
}

// flatpakOutcome separates "not permitted" (the user can run it themselves)
// from a real error.
func (s System) flatpakOutcome(err error, it Item, manual string) Outcome {
	switch {
	case err == nil:
		return Outcome{}
	case isNoPrivilege(err):
		if it.File != "" {
			if kept, kerr := s.keep(it.File); kerr == nil {
				manual = "flatpak install " + kept
			}
		}
		return Outcome{ActionNeeded: "flatpak wasn't allowed to change the system installation; run:\n" + manual}
	}
	return Outcome{Err: err}
}
