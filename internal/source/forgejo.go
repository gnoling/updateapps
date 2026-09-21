package source

import (
	"context"
	"errors"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// Forgejo also covers Gitea; they share the /api/v1 releases API.
type Forgejo struct{}

type fjRelease struct {
	TagName string    `json:"tag_name"`
	Draft   bool      `json:"draft"`
	Assets  []fjAsset `json:"assets"`
}

type fjAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

func (a fjAsset) assetName() string { return a.Name }

func (Forgejo) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	s := app.Source
	var releases []fjRelease
	if err := c.GetJSON(ctx, baseURL(s.Host)+"/api/v1/repos/"+s.Repo+"/releases?limit=10", nil, &releases); err != nil {
		return nil, err
	}
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		asset, err := pickAsset(app.Asset, rel.Assets)
		if err != nil {
			return nil, err
		}
		// The URL is the version (as bash fj_get did): nightly repos reuse tags.
		display := rel.TagName
		if display == "" {
			display = asset.Name
		}
		return &Result{Version: asset.URL, Display: display, URL: asset.URL, Filename: asset.Name, Size: asset.Size}, nil
	}
	return nil, errors.New("no releases found")
}
