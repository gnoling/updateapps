package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
	"github.com/gnoling/updateapps/internal/sh"
)

// HTML scrapes a page (or a command's output) with a CSS selector.
type HTML struct{}

// commandTimeout bounds html.command (e.g. a headless browser render).
const commandTimeout = 2 * time.Minute

func (HTML) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	s := app.Source
	var body []byte
	var base *url.URL
	if s.Command != "" {
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		var stderr bytes.Buffer
		cmd := sh.Command(cctx, s.Command)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("command failed: %w\n%s", err, lastLines(stderr.String(), 5))
		}
		body = out
	} else {
		resp, err := c.Do(ctx, http.MethodGet, s.URL, s.Headers)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if body, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20)); err != nil {
			return nil, err
		}
		base = resp.Request.URL // after redirects
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var values []string
	seen := map[string]bool{}
	doc.Find(s.Select).Each(func(_ int, sel *goquery.Selection) {
		v := strings.TrimSpace(sel.Text())
		if s.Attr != "" {
			v = strings.TrimSpace(sel.AttrOr(s.Attr, ""))
		}
		if v != "" && !seen[v] {
			seen[v] = true
			values = append(values, v)
		}
	})
	found := len(values)
	if values, err = filterValues(values, s.IncludeRegex, s.ExcludeRegex); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		if found > 0 {
			return nil, fmt.Errorf("selector %q matched %d value(s) but the include/exclude filters removed them all", s.Select, found)
		}
		return nil, fmt.Errorf("selector %q matched nothing (page layout changed?)", s.Select)
	}
	raw := values[0]
	if s.Pick == "last" {
		raw = values[len(values)-1]
	}

	value := raw
	if base != nil {
		if ref, err := url.Parse(browserSlashes(raw)); err == nil {
			value = base.ResolveReference(ref).String()
		}
	}
	version, err := htmlVersion(s.Version, value)
	if err != nil {
		return nil, err
	}
	download := value
	if s.Download != "" {
		download, err = expand(s.Download, map[string]string{
			"Value": value, "Raw": raw, "Basename": urlBasename(value), "Version": version,
		})
		if err != nil {
			return nil, err
		}
	}
	return &Result{Version: version, URL: download, Filename: urlBasename(download)}, nil
}

// browserSlashes treats backslashes in a link's path as slashes, as browsers
// do (href='releases\app.tgz').
func browserSlashes(ref string) string {
	end := strings.IndexAny(ref, "?#")
	if end < 0 {
		end = len(ref)
	}
	return strings.ReplaceAll(ref[:end], `\`, "/") + ref[end:]
}

func htmlVersion(mode, value string) (string, error) {
	switch {
	case mode == "" || mode == "basename":
		return urlBasename(value), nil
	case mode == "full":
		return value, nil
	}
	re, err := regexp.Compile(strings.TrimPrefix(mode, "regex:"))
	if err != nil {
		return "", err
	}
	m := re.FindStringSubmatch(value)
	if len(m) < 2 || m[1] == "" {
		return "", fmt.Errorf("version regex %q doesn't match %q", re, value)
	}
	return m[1], nil
}

func filterValues(values []string, include, exclude string) ([]string, error) {
	var inc, exc *regexp.Regexp
	var err error
	if include != "" {
		if inc, err = regexp.Compile(include); err != nil {
			return nil, err
		}
	}
	if exclude != "" {
		if exc, err = regexp.Compile(exclude); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, v := range values {
		if (inc == nil || inc.MatchString(v)) && (exc == nil || !exc.MatchString(v)) {
			out = append(out, v)
		}
	}
	return out, nil
}

// expand renders a definition's download template.
func expand(tmpl string, vars map[string]string) (string, error) {
	t, err := template.New("download").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "", errors.New("download template produced an empty URL")
	}
	return b.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
