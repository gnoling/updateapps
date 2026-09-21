package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

type GitHubRelease struct{}

type ghRelease struct {
	Name        string    `json:"name"`
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   string    `json:"created_at"`
	PublishedAt string    `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	UpdatedAt string `json:"updated_at"`
	URL       string `json:"browser_download_url"`
	Digest    string `json:"digest"`
}

func (a ghAsset) assetName() string { return a.Name }

func (GitHubRelease) Resolve(ctx context.Context, c *fetch.Client, app *def.App, cache *Cache) (*Result, error) {
	s := app.Source
	etag := ""
	if cache != nil {
		etag = cache.ETag
	}

	var rel *ghRelease
	var newETag string
	var notModified bool
	var err error
	if s.Channel == "tag" {
		rel = &ghRelease{}
		newETag, notModified, err = c.GitHub(ctx, "/repos/"+s.Repo+"/releases/tags/"+url.PathEscape(s.Tag), etag, rel)
		var se *fetch.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			// Rolling tags are deleted and recreated by upstream CI.
			return nil, softf("release %q not found (rolling tag being recreated?)", s.Tag)
		}
	} else {
		// Not /releases/latest, which hides prereleases.
		var list []ghRelease
		newETag, notModified, err = c.GitHub(ctx, "/repos/"+s.Repo+"/releases?per_page=100", etag, &list)
		if err == nil && !notModified {
			// tags is the candidate order: the listing's, unless latestFirst
			// has a better one. Releases past page 1 are fetched as they're
			// reached.
			byTag := make(map[string]*ghRelease, len(list))
			tags := make([]string, len(list))
			for i := range list {
				byTag[list[i].TagName], tags[i] = &list[i], list[i].TagName
			}
			if better := latestFirst(ctx, c, s.Repo, list); better != nil {
				tags = better
				// Page 1's ETag can't vouch for an order that came from
				// elsewhere.
				newETag = ""
			}
			var firstErr error
			for _, tag := range tags {
				r := byTag[tag]
				if r == nil { // beyond the first page of the listing
					r = &ghRelease{}
					if _, _, err := c.GitHub(ctx, "/repos/"+s.Repo+"/releases/tags/"+url.PathEscape(tag), "", r); err != nil {
						return nil, err
					}
				}
				if r.Draft || (s.Channel == "stable" && r.Prerelease) {
					continue
				}
				if !s.SkipUnmatched {
					rel = r
					break
				}
				if _, aerr := pickAsset(app.Asset, r.Assets); aerr == nil {
					rel = r
					break
				} else if firstErr == nil {
					firstErr = aerr
				}
				fetch.Logf(ctx, fetch.LogDetail, "skipped release %s: no matching asset", r.TagName)
			}
			switch {
			case rel != nil:
			case firstErr != nil:
				return nil, fmt.Errorf("none of the last %d releases has a matching asset; newest: %w", len(tags), firstErr)
			case s.Channel == "stable":
				return nil, errors.New("no stable (non-prerelease) releases found")
			default:
				return nil, errors.New("no releases found")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if notModified {
		return &Result{Version: cache.Version, Display: cache.Version, NotModified: true,
			Cache: &Cache{ETag: cache.ETag, Version: cache.Version}}, nil
	}

	asset, err := pickAsset(app.Asset, rel.Assets)
	if err != nil {
		return nil, err
	}

	res := &Result{URL: asset.URL, Filename: asset.Name, Size: asset.Size, SHA256: sha256Digest(asset.Digest)}
	switch s.VersionFrom {
	case "tag":
		res.Version = rel.TagName
	case "published_at":
		res.Version = rel.PublishedAt
	case "asset.name":
		res.Version = asset.Name
	case "asset.updated_at":
		res.Version = asset.UpdatedAt
	default: // release: matches bash gh_need_update's name+publishedAt+tagName key
		res.Version = rel.TagName + "|" + rel.Name + "|" + rel.PublishedAt
		res.Display = rel.TagName
	}
	if res.Version == "" {
		return nil, errors.New("release has no value for version_from: " + s.VersionFrom)
	}
	if res.Display == "" {
		res.Display = res.Version
	}
	if newETag != "" {
		res.Cache = &Cache{ETag: newETag, Version: res.Version}
	}
	return res, nil
}

// latestFirst orders releases when REST can't. REST sorts by created_at, the
// tagged commit's date. Repos that tag every release on one commit (RPCS3
// binaries, xenia-canary) tie on it, list in arbitrary order, and may have the
// newest pages away. published_at is no substitute: one project re-published
// its history in reverse. On a tie this asks GraphQL, which sorts by the
// release's real creation time, as `gh release list` does. It returns tags
// newest first, or nil if the REST order stands.
func latestFirst(ctx context.Context, c *fetch.Client, repo string, list []ghRelease) []string {
	newest, tied := "", 0
	for _, r := range list {
		switch {
		case r.CreatedAt > newest:
			newest, tied = r.CreatedAt, 1
		case r.CreatedAt == newest:
			tied++
		}
	}
	if tied < 2 {
		return nil
	}

	if c.GitHubToken != "" {
		owner, name, _ := strings.Cut(repo, "/")
		var out struct {
			Repository struct {
				Releases struct {
					Nodes []struct {
						TagName string `json:"tagName"`
					} `json:"nodes"`
				} `json:"releases"`
			} `json:"repository"`
		}
		const query = `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){
			releases(first:30,orderBy:{field:CREATED_AT,direction:DESC}){nodes{tagName}}}}`
		err := c.GitHubGraphQL(ctx, query, map[string]any{"owner": owner, "name": name}, &out)
		if err == nil && len(out.Repository.Releases.Nodes) > 0 {
			fetch.Logf(ctx, fetch.LogDetail, "%d releases share a created_at; ordered by true creation time (GraphQL)", tied)
			tags := make([]string, len(out.Repository.Releases.Nodes))
			for i, n := range out.Repository.Releases.Nodes {
				tags[i] = n.TagName
			}
			return tags
		}
		fetch.Logf(ctx, fetch.LogDetail, "GraphQL ordering unavailable (%v); falling back to publish time", err)
	}

	// No token: publish time among the ties, plus GitHub's "latest" flag,
	// which sees past page 1.
	ordered := append([]ghRelease(nil), list...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].CreatedAt != ordered[j].CreatedAt {
			return ordered[i].CreatedAt > ordered[j].CreatedAt
		}
		return ordered[i].PublishedAt > ordered[j].PublishedAt
	})
	var tags []string
	var flagged ghRelease
	if _, _, err := c.GitHub(ctx, "/repos/"+repo+"/releases/latest", "", &flagged); err == nil &&
		flagged.CreatedAt >= newest && flagged.PublishedAt > ordered[0].PublishedAt {
		tags = append(tags, flagged.TagName)
	}
	for _, r := range ordered {
		tags = append(tags, r.TagName)
	}
	return tags
}
