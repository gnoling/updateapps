package repo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gnoling/updateapps/internal/config"
	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/install"
)

// Meta is the bookkeeping kept beside a fetched repository.
type Meta struct {
	URL      string    `json:"url"`
	ETag     string    `json:"etag,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	PulledAt time.Time `json:"pulled_at"`
}

// marker identifies a folder as fetched by updateapps and holds its Meta. A
// pull never replaces a folder without it.
const marker = ".updateapps-fetched.json"

func readMeta(dir string) (Meta, error) {
	var m Meta
	data, err := os.ReadFile(filepath.Join(dir, marker))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}

// ArchiveURL is where a repository's branch downloads as one archive. GitHub,
// Forgejo/Gitea and GitLab build one on request, so publishing needs no
// release or CI.
func ArchiveURL(r config.Repository, githubAPI string) (string, error) {
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad url %q", r.URL)
	}
	lower := strings.ToLower(u.Path)
	for _, ext := range []string{".tar.gz", ".tgz", ".tar.xz", ".tar.zst", ".tar.bz2", ".zip", ".tar"} {
		if strings.HasSuffix(lower, ext) {
			return r.URL, nil // already an archive, hosted anywhere
		}
	}
	slug := strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/")
	if strings.Count(slug, "/") < 1 {
		return "", fmt.Errorf("url %q isn't a repository (owner/name) or an archive", r.URL)
	}
	switch {
	case u.Host == "github.com":
		// The API, so a token reaches private repositories. An empty ref is
		// the default branch.
		return strings.TrimSuffix(githubAPI, "/") + "/repos/" + slug + "/tarball/" + url.PathEscape(r.Branch), nil
	case strings.Contains(u.Host, "gitlab"):
		api := u.Scheme + "://" + u.Host + "/api/v4/projects/" + url.PathEscape(slug) + "/repository/archive.tar.gz"
		if r.Branch != "" {
			api += "?sha=" + url.QueryEscape(r.Branch)
		}
		return api, nil
	}
	branch := r.Branch // Forgejo / Gitea
	if branch == "" {
		branch = "main"
	}
	return u.Scheme + "://" + u.Host + "/" + slug + "/archive/" + url.PathEscape(branch) + ".tar.gz", nil
}

// Change summarizes what a pull did to a repository's definitions.
type Change struct {
	UpToDate                bool
	Commit                  string
	Added, Changed, Removed []string
	// NowNeedTrust lists definitions that newly need a trusted repository.
	NowNeedTrust []string
}

// Pull fetches a url: repository into apps.d/<name>/: its definitions and
// repo.yaml only. The old copy stays until the new one has loaded.
func Pull(ctx context.Context, c *fetch.Client, cfg *config.Config, r config.Repository) (*Change, error) {
	if r.URL == "" {
		return nil, errors.New("it has no url:, so its folder is yours to manage")
	}
	vars := cfg.Vars()
	archive, err := ArchiveURL(r, c.GitHubAPI)
	if err != nil {
		return nil, err
	}
	dest := Dir(cfg, r)
	meta, metaErr := readMeta(dest)
	if _, err := os.Stat(dest); err == nil && metaErr != nil {
		return nil, fmt.Errorf("%s exists and wasn't fetched by updateapps; move it or rename the repository", dest)
	}
	var headers map[string]string
	if metaErr == nil && meta.ETag != "" && meta.URL == archive {
		headers = map[string]string{"If-None-Match": meta.ETag}
	}
	if err := os.MkdirAll(cfg.Definitions, 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(cfg.Definitions, "."+r.Name+"-pull-") // beside dest: the swap is a rename
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	file := filepath.Join(work, "archive")
	var etag string
	err = c.Download(ctx, archive, file, fetch.DownloadOpts{Headers: headers, ETag: &etag})
	if errors.Is(err, fetch.ErrNotModified) {
		meta.PulledAt = time.Now()
		writeMeta(dest, meta)
		return &Change{UpToDate: true, Commit: meta.Commit}, nil
	}
	if err != nil {
		return nil, err
	}
	unpacked := filepath.Join(work, "files")
	if err := install.ExtractArchive(ctx, file, unpacked, func(string, ...any) {}); err != nil {
		return nil, err
	}
	// Host archives wrap everything in one folder (owner-name-commit/).
	if entries, err := os.ReadDir(unpacked); err == nil && len(entries) == 1 && entries[0].IsDir() {
		unpacked = filepath.Join(unpacked, entries[0].Name())
	}

	change := &Change{Commit: archiveCommit(file)}
	fresh := Info{Repository: r, Dir: unpacked}
	newApps, problem, errs := loadOne(&fresh, vars)
	if problem != "" {
		return nil, errors.New(problem)
	}
	if len(newApps) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("no usable definitions in it; first problem: %w", errs[0])
		}
		return nil, errors.New("no definitions in it (expected *.yaml in apps.d/ or at the top level)")
	}
	old := Info{Repository: r, Dir: dest}
	oldApps, _, _ := loadOne(&old, vars)
	change.diff(oldApps, newApps, vars, r.Name)

	// Keep only the definitions folder, with the manifest beside them.
	if fresh.DefsDir != unpacked {
		if data, err := os.ReadFile(filepath.Join(unpacked, "repo.yaml")); err == nil {
			os.WriteFile(filepath.Join(fresh.DefsDir, "repo.yaml"), data, 0o644)
		}
		unpacked = fresh.DefsDir
	}
	writeMeta(unpacked, Meta{URL: archive, ETag: etag, Commit: change.Commit, PulledAt: time.Now()})

	trash := filepath.Join(work, "previous")
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, trash); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(unpacked, dest); err != nil {
		os.Rename(trash, dest) // put the old one back
		return nil, err
	}
	return change, nil
}

func writeMeta(dir string, m Meta) {
	if data, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(filepath.Join(dir, marker), append(data, '\n'), 0o644)
	}
}

// diff compares definitions by file content, so notes-only edits count too.
func (c *Change) diff(oldApps, newApps []*def.App, vars def.Vars, repo string) {
	read := func(a *def.App) []byte { data, _ := os.ReadFile(a.Path); return data }
	before := map[string]*def.App{}
	for _, a := range oldApps {
		before[a.ID] = a
	}
	for _, a := range newApps {
		prev, existed := before[a.ID]
		delete(before, a.ID)
		switch {
		case !existed:
			c.Added = append(c.Added, a.ID)
		case !bytes.Equal(read(prev), read(a)):
			c.Changed = append(c.Changed, a.ID)
		default:
			continue
		}
		if needsTrust(a, vars, repo) != "" && (!existed || needsTrust(prev, vars, repo) == "") {
			c.NowNeedTrust = append(c.NowNeedTrust, a.ID)
		}
	}
	for id := range before {
		c.Removed = append(c.Removed, id)
	}
	sort.Strings(c.Removed)
}

// archiveCommit reads the commit id git archive stores in the tarball's pax
// header.
func archiveCommit(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return ""
	}
	h, err := tar.NewReader(gz).Next()
	if err != nil || h.Typeflag != tar.TypeXGlobalHeader {
		return ""
	}
	return h.PAXRecords["comment"]
}
