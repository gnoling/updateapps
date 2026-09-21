// Package fetch is the shared HTTP layer: retries, rate limits, GitHub auth
// and streaming downloads.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	GitHubAPI = "https://api.github.com"
	userAgent = "updateapps (+https://github.com/gnoling/updateapps)"
)

// Client is safe for concurrent use.
type Client struct {
	HTTP         *http.Client
	GitHubAPI    string // overridable for tests
	GitHubToken  string
	MaxRetries   int           // retries after the first attempt
	Backoff      time.Duration // first retry delay; doubles each retry
	MaxWait      time.Duration // longest Retry-After / rate-limit reset we'll sit out
	StallTimeout time.Duration // abort a download that receives nothing for this long
}

func New(token string) *Client {
	return &Client{
		HTTP:         &http.Client{Transport: transport(false)},
		GitHubAPI:    GitHubAPI,
		GitHubToken:  token,
		MaxRetries:   3,
		Backoff:      time.Second,
		MaxWait:      time.Minute,
		StallTimeout: time.Minute,
	}
}

// Insecure returns a copy that skips TLS verification (insecure: true).
func (c *Client) Insecure() *Client {
	cp := *c
	cp.HTTP = &http.Client{Transport: transport(true)}
	return &cp
}

func transport(insecure bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	t.MaxIdleConnsPerHost = 8
	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return t
}

// StatusError is a non-2xx response.
type StatusError struct {
	URL    string
	Code   int
	Status string
	Body   string // first part of the body, for diagnostics
}

func (e *StatusError) Error() string {
	msg := e.Status
	if detail := apiMessage(e.Body); detail != "" {
		msg += ": " + detail
	}
	return msg
}

func apiMessage(body string) string {
	var v struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &v) == nil {
		return v.Message
	}
	return ""
}

type logKey struct{}

// Job log levels: always shown, -v, -vv (HTTP requests).
const (
	LogAlways = iota
	LogDetail
	LogTrace
)

// WithLogger attaches the job's logger to ctx, for this client and Lua
// scripts.
func WithLogger(ctx context.Context, log func(level int, format string, args ...any)) context.Context {
	return context.WithValue(ctx, logKey{}, log)
}

// Logf writes to the job logger on ctx, if there is one.
func Logf(ctx context.Context, level int, format string, args ...any) {
	if f, ok := ctx.Value(logKey{}).(func(int, string, ...any)); ok {
		f(level, format, args...)
	}
}

func logf(ctx context.Context, format string, args ...any) { Logf(ctx, LogTrace, format, args...) }

// Do sends a body-less request; see DoBody.
func (c *Client) Do(ctx context.Context, method, rawURL string, headers map[string]string) (*http.Response, error) {
	return c.DoBody(ctx, method, rawURL, headers, nil)
}

// DoBody sends a request, retrying connection errors, 5xx, 429 and short
// rate-limit waits. A non-2xx result is a *StatusError; 304 is returned as a
// response.
func (c *Client) DoBody(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		var payload io.Reader
		if body != nil {
			payload = bytes.NewReader(body) // fresh reader per attempt
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, payload)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		if c.isGitHubAPI(req.URL) {
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			if c.GitHubToken != "" {
				req.Header.Set("Authorization", "Bearer "+c.GitHubToken)
			}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.HTTP.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
		case resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified:
			logf(ctx, "%s %s -> %s", method, rawURL, resp.Status)
			return resp, nil
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			serr := &StatusError{URL: rawURL, Code: resp.StatusCode, Status: resp.Status, Body: string(body)}
			logf(ctx, "%s %s -> %s", method, rawURL, resp.Status)
			var retry bool
			retry, wait = c.retryable(resp)
			if !retry {
				return nil, serr
			}
			lastErr = serr
		}

		if attempt >= c.MaxRetries {
			return nil, lastErr
		}
		if wait == 0 {
			wait = c.Backoff << attempt
		}
		logf(ctx, "retrying in %s: %v", wait.Round(time.Millisecond), lastErr)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// retryable reports whether to retry and how long the server asked to wait (0
// = backoff).
func (c *Client) retryable(resp *http.Response) (bool, time.Duration) {
	code := resp.StatusCode
	limited := code == http.StatusTooManyRequests ||
		(code == http.StatusForbidden && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"))
	if limited {
		var wait time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s) * time.Second
		} else if ts, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && resp.Header.Get("X-RateLimit-Remaining") == "0" {
			wait = time.Until(time.Unix(ts, 0)) + time.Second
		}
		// Don't hold a worker through a long reset window; the next run
		// retries.
		return wait <= c.MaxWait, max(wait, 0)
	}
	return code >= 500, 0
}

func (c *Client) isGitHubAPI(u *url.URL) bool {
	base, err := url.Parse(c.GitHubAPI)
	return err == nil && u.Host == base.Host
}

// GetJSON fetches url and decodes the JSON body into v.
func (c *Client) GetJSON(ctx context.Context, rawURL string, headers map[string]string, v any) error {
	resp, err := c.Do(ctx, http.MethodGet, rawURL, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// GitHub GETs an API path into v. A non-empty etag makes it conditional: on
// 304 notModified is true, v is untouched and the rate limit isn't charged.
func (c *Client) GitHub(ctx context.Context, path, etag string, v any) (newETag string, notModified bool, err error) {
	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-None-Match": etag}
	}
	resp, err := c.Do(ctx, http.MethodGet, strings.TrimSuffix(c.GitHubAPI, "/")+path, headers)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
			return "", false, fmt.Errorf("GitHub rejected the token (401); check github_token / $GITHUB_TOKEN")
		}
		if errors.As(err, &se) && se.Code == http.StatusForbidden && c.GitHubToken == "" {
			return "", false, fmt.Errorf("%w (no GitHub token configured; unauthenticated requests are limited to 60/hour)", err)
		}
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return etag, true, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return "", false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return resp.Header.Get("ETag"), false, nil
}

// DownloadOpts are Download's optional extras.
type DownloadOpts struct {
	Headers  map[string]string
	Progress func(done, total int64) // total is -1 when the server doesn't say
	SHA256   string                  // expected hex digest; "" skips the check
	ETag     *string                 // if set, receives the response's ETag
}

// ErrNotModified: a conditional Download got 304.
var ErrNotModified = errors.New("not modified")

// ErrDigest means the bytes received aren't the bytes upstream published.
var ErrDigest = errors.New("sha256 mismatch")

// GitHubGraphQL runs a GraphQL query (token required) and decodes "data" into out.
func (c *Client) GitHubGraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	if c.GitHubToken == "" {
		return errors.New("the GitHub GraphQL API needs a token")
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	resp, err := c.DoBody(ctx, http.MethodPost, strings.TrimSuffix(c.GitHubAPI, "/")+"/graphql",
		map[string]string{"Content-Type": "application/json"}, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decoding GraphQL response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, out)
}

// Download streams url to path, restarting an interrupted transfer up to
// MaxRetries times.
func (c *Client) Download(ctx context.Context, rawURL, path string, opts DownloadOpts) error {
	var err error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			logf(ctx, "download interrupted (%v); restarting", err)
		}
		if err = c.downloadOnce(ctx, rawURL, path, opts); err == nil {
			return nil
		}
		var se *StatusError
		if ctx.Err() != nil || errors.As(err, &se) || errors.Is(err, errLocal) || errors.Is(err, ErrDigest) || errors.Is(err, ErrNotModified) {
			break // Do already retried what's retryable
		}
	}
	os.Remove(path)
	return err
}

var errLocal = errors.New("local write failed")

func (c *Client) downloadOnce(parent context.Context, rawURL, path string, opts DownloadOpts) error {
	// Downloads have no overall timeout, so a watchdog cancels stalled ones.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watchdog := time.AfterFunc(c.StallTimeout, cancel)
	defer watchdog.Stop()

	resp, err := c.Do(ctx, http.MethodGet, rawURL, opts.Headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return ErrNotModified
	}
	if opts.ETag != nil {
		*opts.ETag = resp.Header.Get("ETag")
	}
	sum := sha256.New()
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("%w: %v", errLocal, err)
	}
	defer f.Close()

	buf := make([]byte, 256<<10)
	var done int64
	for {
		n, rerr := resp.Body.Read(buf)
		watchdog.Reset(c.StallTimeout)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return fmt.Errorf("%w: %v", errLocal, werr)
			}
			sum.Write(buf[:n])
			done += int64(n)
			if opts.Progress != nil {
				opts.Progress(done, resp.ContentLength)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if parent.Err() != nil {
				return parent.Err()
			}
			if ctx.Err() != nil {
				return fmt.Errorf("transfer stalled for %s", c.StallTimeout)
			}
			return rerr
		}
	}
	if resp.ContentLength >= 0 && done != resp.ContentLength {
		return fmt.Errorf("short download: got %d of %d bytes", done, resp.ContentLength)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); opts.SHA256 != "" && !strings.EqualFold(got, opts.SHA256) {
		return fmt.Errorf("%w: upstream published %s, received %s", ErrDigest, opts.SHA256, got)
	}
	return f.Close()
}

// GitHubToken resolves the token: config, $GITHUB_TOKEN/$GH_TOKEN, then `gh
// auth token` if gh is installed.
func GitHubToken(configured string) string {
	if configured != "" {
		return configured
	}
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if gh, err := exec.LookPath("gh"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, gh, "auth", "token").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
