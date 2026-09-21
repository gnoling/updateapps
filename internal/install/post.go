package install

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/sh"
)

// HookTimeout bounds each post hook, so a hung ssh can't hold a worker forever.
var HookTimeout = 10 * time.Minute

// HookEnv is what post hooks see on top of the process environment.
type HookEnv struct {
	File    string // the download: its installed path, or the staged file for extract/none
	Version string
	Vars    def.Vars
}

// RunPost runs the post hooks in order, stopping at the first failure. Output
// goes to logf.
func RunPost(ctx context.Context, app *def.App, env HookEnv, stage string, logf func(string, ...any)) error {
	dir := stage
	switch app.Install.Type {
	case def.InstallExtract:
		dir = app.Install.Dest
	case def.InstallFile, def.InstallExtractOne:
		dir = filepath.Dir(app.Install.Dest)
	}
	environ := append(os.Environ(),
		"FILE="+env.File, "DEST="+app.Install.Dest, "VERSION="+env.Version, "NAME="+app.ID,
		"APPDIR="+env.Vars.AppDir, "APPIMAGEDIR="+env.Vars.AppImageDir)

	for i, hook := range app.Post {
		logf("post[%d]: %s", i+1, hook)
		hctx, cancel := context.WithTimeout(ctx, HookTimeout)
		cmd := sh.Command(hctx, hook)
		cmd.Dir, cmd.Env = dir, environ
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		timedOut := hctx.Err() == context.DeadlineExceeded
		cancel()

		text := strings.TrimSpace(out.String())
		for _, line := range strings.Split(text, "\n") {
			if line != "" {
				logf("  %s", line)
			}
		}
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case timedOut:
			return fmt.Errorf("post hook %d timed out after %s: %s", i+1, HookTimeout, hook)
		case err != nil:
			lines := strings.Split(text, "\n")
			if len(lines) > 5 {
				lines = lines[len(lines)-5:]
			}
			return fmt.Errorf("post hook %d failed (%v): %s\n%s", i+1, err, hook, strings.Join(lines, "\n"))
		}
	}
	return nil
}
