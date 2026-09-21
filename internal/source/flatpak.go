package source

import (
	"context"
	"io"
	"net/http"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/flatpak"
)

// Flatpak tracks an app that flatpak updates from a remote. The version is the
// commit the remote offers; updating means asking flatpak to update (or first
// install) that app.
type Flatpak struct {
	UserScope bool // default scope when the definition doesn't set install.scope
}

func (f Flatpak) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	if err := flatpak.Available(); err != nil {
		return nil, err
	}
	s := app.Source
	user := f.UserScope
	if app.Install.Scope != "" {
		user = app.Install.Scope == "user"
	}

	ref := flatpak.Ref{ID: s.App, Branch: s.Branch, Remote: s.Remote}
	if ref.Branch == "" {
		ref.Branch = "stable"
	}
	res := &Result{}
	if s.Ref != "" {
		resp, err := c.Do(ctx, http.MethodGet, s.Ref, nil)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if ref, err = flatpak.ParseRef(data); err != nil {
			return nil, err
		}
		// The ref file is what a first install is made from.
		res.URL, res.Filename = s.Ref, urlBasename(s.Ref)
	}

	commit, origin, installed := flatpak.Installed(ctx, user, ref)
	if installed && origin != "" {
		ref.Remote = origin
	}
	res.Installed = commit
	if !installed && s.Ref != "" {
		// The ref's remote isn't configured until flatpak installs from it;
		// the installer records the real commit.
		res.Version, res.Display = "not-installed:"+ref.String(), "not installed"
		return res, nil
	}
	remote, err := flatpak.RemoteInfo(ctx, user, ref.Remote, ref)
	if err != nil {
		return nil, err
	}
	res.Version, res.Display = remote.Commit, flatpak.Display(remote.Version, remote.Commit)
	return res, nil
}
