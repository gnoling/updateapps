package source

import (
	"context"
	"errors"
	"net/url"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

type GitLab struct{}

type glRelease struct {
	TagName string `json:"tag_name"`
	Assets  struct {
		Links []glLink `json:"links"`
	} `json:"assets"`
}

type glLink struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Direct string `json:"direct_asset_url"`
}

func (l glLink) assetName() string { return l.Name }

func (GitLab) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	s := app.Source
	host := s.Host
	if host == "" {
		host = "gitlab.com"
	}
	var releases []glRelease
	api := baseURL(host) + "/api/v4/projects/" + url.PathEscape(s.Project) + "/releases?per_page=1"
	if err := c.GetJSON(ctx, api, nil, &releases); err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, errors.New("no releases found")
	}
	rel := releases[0]
	link, err := pickAsset(app.Asset, rel.Assets.Links)
	if err != nil {
		return nil, err
	}
	dl := link.Direct
	if dl == "" {
		dl = link.URL
	}
	// Link names are labels, not file names; keep the URL's name for the download.
	return &Result{Version: rel.TagName, URL: dl, Filename: urlBasename(dl)}, nil
}
