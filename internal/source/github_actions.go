package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// GitHubActions resolves the newest workflow run's artifact (token required).
// Artifacts are zips, except single files uploaded un-zipped, which arrive as
// themselves.
type GitHubActions struct{}

type ghWorkflow struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type ghRun struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
	CreatedAt  string `json:"created_at"`
}

// fromPullRequest: the run built a proposed change, possibly from a fork.
func (r ghRun) fromPullRequest() bool {
	return r.Event == "pull_request" || r.Event == "pull_request_target"
}

type ghArtifact struct {
	Name    string `json:"name"`
	Size    int64  `json:"size_in_bytes"`
	Expired bool   `json:"expired"`
	URL     string `json:"archive_download_url"`
	Digest  string `json:"digest"`
}

// StaleRetryDelay is how long to wait before asking GitHub again when its
// answer looks stale.
var StaleRetryDelay = 3 * time.Second

// staleError marks an answer that may be stale: GitHub's run listings are
// eventually consistent and have returned month-old runs.
type staleError struct {
	err error // what to report if asking again doesn't help
}

func (e *staleError) Error() string { return e.err.Error() }

func (g GitHubActions) Resolve(ctx context.Context, c *fetch.Client, app *def.App, cache *Cache) (*Result, error) {
	if c.GitHubToken == "" {
		return nil, errors.New("GitHub Actions artifacts need a token (set github_token or $GITHUB_TOKEN)")
	}
	res, err := g.resolve(ctx, c, app, cache)
	var stale *staleError
	if errors.As(err, &stale) {
		fetch.Logf(ctx, fetch.LogDetail, "answer looks stale (%v); asking again", stale.err)
		select {
		case <-time.After(StaleRetryDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if res, err = g.resolve(ctx, c, app, cache); errors.As(err, &stale) {
			return nil, stale.err
		}
	}
	return res, err
}

func (GitHubActions) resolve(ctx context.Context, c *fetch.Client, app *def.App, cache *Cache) (*Result, error) {
	s := app.Source
	workflows, err := workflowRefs(ctx, c, s.Repo, s.Workflow)
	if err != nil {
		return nil, err
	}
	// Branch, event and success are judged here, not passed as query
	// parameters: those move the request to GitHub's search-backed path, which
	// is eventually consistent (seen a month stale). The filtered query is
	// only a fallback when 300 runs hold no match.
	wanted := func(r ghRun) bool {
		switch {
		case s.Status == "success" && r.Conclusion != "success":
		case s.Branch != "" && r.HeadBranch != s.Branch:
		// Never install a pull request's build unless the definition asks for
		// that event.
		case s.Event == "" && r.fromPullRequest():
		case s.Event != "" && r.Event != s.Event:
		default:
			return true
		}
		return false
	}
	listRuns := func(wf string, q url.Values) ([]ghRun, error) {
		var page struct {
			Runs []ghRun `json:"workflow_runs"`
		}
		_, _, err := c.GitHub(ctx, "/repos/"+s.Repo+"/actions/workflows/"+wf+"/runs?"+q.Encode(), "", &page)
		return page.Runs, err
	}
	// Several workflows can share a name; the newest run across them wins.
	var run *ghRun
	consider := func(runs []ghRun) {
		for i, r := range runs {
			if wanted(r) && (run == nil || r.ID > run.ID) {
				run = &runs[i]
			}
		}
	}
	for page := 1; page <= 3 && run == nil; page++ {
		more := false
		for _, wf := range workflows {
			runs, err := listRuns(wf, url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}})
			if err != nil {
				return nil, err
			}
			consider(runs)
			more = more || len(runs) == 100
		}
		if !more {
			break
		}
	}
	if run == nil {
		q := url.Values{"per_page": {"100"}}
		if s.Status == "success" {
			q.Set("status", "success")
		}
		if s.Branch != "" {
			q.Set("branch", s.Branch)
		}
		if s.Event != "" {
			q.Set("event", s.Event)
		}
		for _, wf := range workflows {
			runs, err := listRuns(wf, q)
			if err != nil {
				return nil, err
			}
			consider(runs)
		}
	}
	if run == nil {
		return nil, fmt.Errorf("workflow %q has no matching runs", s.Workflow)
	}
	if cache != nil {
		installed, _, _ := strings.Cut(cache.Installed, "|")
		id, err := strconv.ParseInt(installed, 10, 64)
		switch {
		case err != nil:
		case run.ID < id:
			// Run ids only grow, so never move backwards from what's installed.
			return nil, &staleError{softf("GitHub listed run %d as latest, older than installed run %d; ignoring the stale answer", run.ID, id)}
		case run.ID == id:
			// Already installed: skip the artifact lookup, which may have
			// expired.
			return &Result{Version: cache.Installed, NotModified: true}, nil
		}
	}

	var page struct {
		Artifacts []ghArtifact `json:"artifacts"`
	}
	if _, _, err := c.GitHub(ctx, "/repos/"+s.Repo+"/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/artifacts?per_page=100", "", &page); err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if s.ArtifactRegex != "" {
		if re, err = regexp.Compile(s.ArtifactRegex); err != nil {
			return nil, err
		}
	}
	var art *ghArtifact
	names := make([]string, 0, len(page.Artifacts))
	for i, a := range page.Artifacts {
		names = append(names, a.Name)
		if art == nil && (a.Name == s.Artifact || (re != nil && re.MatchString(a.Name))) {
			art = &page.Artifacts[i]
		}
	}
	display := strconv.FormatInt(run.ID, 10)
	switch {
	case art == nil && (run.Status != "completed" || s.Status == "any"):
		// With status: any we may be looking at a run that hasn't uploaded yet.
		return nil, softf("run %s has no %q artifact yet (still building?)", display, s.Artifact+s.ArtifactRegex)
	case art == nil:
		return nil, fmt.Errorf("run %s has no artifact matching %q; it has:\n%s", display, s.Artifact+s.ArtifactRegex, strings.Join(names, "\n"))
	case art.Expired:
		// No good build in 90 days, or this isn't really the latest run.
		return nil, &staleError{fmt.Errorf("artifact %q of run %s has expired (GitHub keeps them 90 days)", art.Name, display)}
	}

	return &Result{
		Version:  display + "|" + run.HeadSHA,
		Display:  display,
		URL:      art.URL,
		Filename: artifactFilename(art.Name),
		Size:     art.Size,
		SHA256:   sha256Digest(art.Digest),
	}, nil
}

// artifactFilename names the download: an artifact named like a file is an
// un-zipped upload, anything else a zip. Only post hooks see the name.
func artifactFilename(name string) string {
	if ext := path.Ext(name); ext != "" && !strings.ContainsAny(ext, " )") {
		return name
	}
	return name + ".zip"
}

// workflowRefs maps the definition's workflow (file or display name) to what
// the runs endpoint accepts.
func workflowRefs(ctx context.Context, c *fetch.Client, repo, workflow string) ([]string, error) {
	if strings.HasSuffix(workflow, ".yml") || strings.HasSuffix(workflow, ".yaml") {
		return []string{url.PathEscape(workflow)}, nil
	}
	var page struct {
		Workflows []ghWorkflow `json:"workflows"`
	}
	if _, _, err := c.GitHub(ctx, "/repos/"+repo+"/actions/workflows?per_page=100", "", &page); err != nil {
		return nil, err
	}
	var refs, names []string
	for _, wf := range page.Workflows {
		names = append(names, wf.Name)
		if strings.EqualFold(wf.Name, workflow) {
			refs = append(refs, strconv.FormatInt(wf.ID, 10))
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no workflow named %q; the repo has:\n%s", workflow, strings.Join(names, "\n"))
	}
	return refs, nil
}
