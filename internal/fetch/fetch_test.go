package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(apiURL string) *Client {
	c := New("tok")
	c.GitHubAPI = apiURL
	c.Backoff = time.Millisecond
	c.MaxWait = 50 * time.Millisecond
	return c
}

func TestRetriesServerErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	var v struct{ OK bool }
	if err := testClient("").GetJSON(context.Background(), srv.URL, nil, &v); err != nil || !v.OK {
		t.Fatalf("err=%v v=%+v", err, v)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := testClient("").Do(context.Background(), "GET", srv.URL, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 || hits.Load() != 4 {
		t.Errorf("err=%v hits=%d (want 503 after 1+3 attempts)", err, hits.Load())
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	_, err := testClient("").Do(context.Background(), "GET", srv.URL, nil)
	if err == nil || hits.Load() != 1 || err.Error() != "404 Not Found: Not Found" {
		t.Errorf("err=%v hits=%d", err, hits.Load())
	}
}

func TestHonorsRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}))
	defer srv.Close()
	if _, err := testClient("").Do(context.Background(), "GET", srv.URL, nil); err != nil || hits.Load() != 2 {
		t.Errorf("err=%v hits=%d", err, hits.Load())
	}
}

// An exhausted GitHub quota that resets far in the future fails fast instead
// of parking a worker.
func TestLongRateLimitFailsFast(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	start := time.Now()
	_, err := testClient("").Do(context.Background(), "GET", srv.URL, nil)
	if err == nil || hits.Load() != 1 || time.Since(start) > time.Second {
		t.Errorf("err=%v hits=%d elapsed=%s", err, hits.Load(), time.Since(start))
	}
}

func TestTokenOnlyGoesToGitHubAPI(t *testing.T) {
	var apiAuth, otherAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	}))
	defer api.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherAuth = r.Header.Get("Authorization")
	}))
	defer other.Close()

	c := testClient(api.URL)
	var v map[string]any
	if _, _, err := c.GitHub(context.Background(), "/repos/o/r/releases", "", &v); err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Do(context.Background(), "GET", other.URL, nil); err == nil {
		resp.Body.Close()
	}
	if apiAuth != "Bearer tok" || otherAuth != "" {
		t.Errorf("api auth=%q other auth=%q", apiAuth, otherAuth)
	}
}

func TestGitHubConditional(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		fmt.Fprint(w, `{"x":1}`)
	}))
	defer srv.Close()
	c := testClient(srv.URL)
	var v map[string]int
	etag, nm, err := c.GitHub(context.Background(), "/x", "", &v)
	if err != nil || nm || etag != `"abc"` || v["x"] != 1 {
		t.Fatalf("first: etag=%q nm=%v err=%v", etag, nm, err)
	}
	etag, nm, err = c.GitHub(context.Background(), "/x", etag, &v)
	if err != nil || !nm || etag != `"abc"` {
		t.Fatalf("second: etag=%q nm=%v err=%v", etag, nm, err)
	}
}

func TestDownloadProgressAndCleanup(t *testing.T) {
	payload := make([]byte, 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
	defer srv.Close()
	c := testClient("")
	dir := t.TempDir()

	var last, total int64
	path := filepath.Join(dir, "ok")
	if err := c.Download(context.Background(), srv.URL, path, DownloadOpts{Progress: func(d, tot int64) { last, total = d, tot }}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Size() != 1<<20 || last != 1<<20 || total != 1<<20 {
		t.Errorf("size=%d last=%d total=%d", info.Size(), last, total)
	}

	bad := filepath.Join(dir, "bad")
	if err := c.Download(context.Background(), srv.URL+"/missing", bad, DownloadOpts{}); err == nil {
		t.Error("expected an error for 404")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("failed download left a file behind")
	}
}

func TestDownloadRestartsTruncatedTransfer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		if hits.Add(1) == 1 {
			w.Write([]byte("01234")) // then the handler returns: connection dies short
			return
		}
		w.Write([]byte("0123456789"))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "f")
	if err := testClient("").Download(context.Background(), srv.URL, path, DownloadOpts{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "0123456789" || hits.Load() != 2 {
		t.Errorf("data=%q hits=%d", data, hits.Load())
	}
}

func TestCancelStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := testClient("")
	c.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, "GET", srv.URL, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

func TestDownloadVerifiesDigest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "f")
	const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if err := testClient("").Download(context.Background(), srv.URL, path, DownloadOpts{SHA256: strings.ToUpper(helloSHA)}); err != nil {
		t.Fatal(err)
	}
	hits.Store(0)
	err := testClient("").Download(context.Background(), srv.URL, path, DownloadOpts{SHA256: strings.Repeat("0", 64)})
	if !errors.Is(err, ErrDigest) || hits.Load() != 1 {
		t.Errorf("err=%v hits=%d (a mismatch is not a transient error; don't re-download)", err, hits.Load())
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("a file that failed verification was left on disk")
	}
}
