package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/luart"
)

// Script is the Lua escape hatch: the script resolves, installing stays
// declarative.
type Script struct{ Vars def.Vars }

func (s Script) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	code, chunk := app.Source.Lua, app.ID+".yaml"
	if app.Source.ScriptFile != "" {
		data, err := os.ReadFile(app.ScriptPath())
		if err != nil {
			return nil, err
		}
		code, chunk = string(data), filepath.Base(app.ScriptPath())
	}
	res, err := luart.Resolve(ctx, luart.Env{Client: c, App: app, Vars: s.Vars}, chunk, code)
	var soft *luart.SoftFailure
	if errors.As(err, &soft) {
		return nil, &SoftError{Msg: soft.Reason}
	}
	if err != nil {
		return nil, err
	}
	filename := res.Filename
	if filename == "" {
		filename = urlBasename(res.URL)
	}
	return &Result{Version: res.Version, Display: res.Display, URL: res.URL, Filename: filename,
		SHA256: res.SHA256, Headers: res.Headers}, nil
}
