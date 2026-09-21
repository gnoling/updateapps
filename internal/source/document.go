package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// Document is the json and yaml source: fetch a document, query it with jq.
type Document struct{ YAML bool }

func (d Document) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	s := app.Source
	resp, err := c.Do(ctx, http.MethodGet, s.URL, s.Headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var doc any
	if d.YAML {
		err = yaml.Unmarshal(body, &doc)
	} else {
		err = json.Unmarshal(body, &doc)
	}
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.URL, err)
	}
	doc = jqValue(doc)

	version, err := JQString(doc, s.Version)
	if err != nil {
		return nil, fmt.Errorf("source.version: %w", err)
	}
	var download string
	if s.URLJQ != "" {
		if download, err = JQString(doc, s.URLJQ); err != nil {
			return nil, fmt.Errorf("source.url_jq: %w", err)
		}
	} else if download, err = expand(s.Download, map[string]string{"Version": version}); err != nil {
		return nil, err
	}
	return &Result{Version: version, URL: download, Filename: urlBasename(download)}, nil
}

// JQString runs expr against value and returns its first output as a string.
func JQString(value any, expr string) (string, error) {
	q, err := gojq.Parse(expr)
	if err != nil {
		return "", err
	}
	v, ok := q.Run(value).Next()
	if !ok || v == nil {
		return "", errors.New("jq expression produced nothing")
	}
	if err, isErr := v.(error); isErr {
		return "", err
	}
	if str, isStr := v.(string); isStr {
		if str == "" {
			return "", errors.New("jq expression produced an empty string")
		}
		return str, nil
	}
	out, err := json.Marshal(v)
	return string(out), err
}

// jqValue rewrites decoded YAML into the types gojq accepts (JSON's).
func jqValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jqValue(e)
		}
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[fmt.Sprint(k)] = jqValue(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = jqValue(e)
		}
	case time.Time:
		return x.Format(time.RFC3339)
	case int64:
		return int(x)
	case uint64:
		return int(x)
	}
	return v
}
