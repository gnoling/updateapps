package source

import (
	"context"
	"net/http"
	"strings"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// HTTPETag watches one fixed URL whose content changes in place.
type HTTPETag struct{}

func (HTTPETag) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	s := app.Source
	resp, err := c.Do(ctx, http.MethodHead, s.URL, s.Headers)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	version := strings.Trim(strings.TrimPrefix(resp.Header.Get("ETag"), "W/"), `"`)
	if version == "" {
		// Not what the type is named for, but it answers the same question.
		version = resp.Header.Get("Last-Modified")
	}
	if version == "" {
		return nil, softf("%s sent neither ETag nor Last-Modified", resp.Request.URL.Host)
	}
	size := resp.ContentLength
	if size < 0 {
		size = 0
	}
	return &Result{Version: version, URL: s.URL, Filename: urlBasename(s.URL), Size: size, Headers: s.Headers}, nil
}
