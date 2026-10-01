package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gnoling/updateapps/internal/def"
)

const (
	shaMaster = "1bcc369e89f08243e0a462882fb1f3e42e51de3a"
	shaDev    = "b7f56efccce8f5eeb995bb35fefa4979d414d033"
	shaTag    = "10e83f5859980a2ed67b8612a082fa9d18688498"
)

// pkt encodes pkt-lines the way git's smart HTTP advertisement does.
func pkt(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		if l == "" {
			b.WriteString("0000")
			continue
		}
		fmt.Fprintf(&b, "%04x%s", len(l)+4, l)
	}
	return b.String()
}

func gitServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/r/info/refs" || r.URL.Query().Get("service") != "git-upload-pack" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if status != 0 {
			http.Error(w, "nope", status)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGit(t *testing.T) {
	smart := pkt("# service=git-upload-pack\n", "",
		shaMaster+" HEAD\x00multi_ack thin-pack symref=HEAD:refs/heads/master agent=git/github\n",
		shaDev+" refs/heads/dev\n",
		shaMaster+" refs/heads/master\n",
		shaTag+" refs/tags/v1\n",
		shaDev+" refs/tags/v1^{}\n", "")
	srv := gitServer(t, smart, 0)
	resolve := func(branch string) (*Result, error) {
		app := &def.App{ID: "r", Source: def.Source{Type: def.SourceGit, URL: srv.URL + "/o/r", Branch: branch}}
		return (Git{}).Resolve(context.Background(), client("", ""), app, nil)
	}

	res, err := resolve("")
	if err != nil || res.Version != shaMaster || res.Display != "master@1bcc369" {
		t.Fatalf("default branch: %+v, %v", res, err)
	}
	res, err = resolve("dev")
	if err != nil || res.Version != shaDev || res.Display != "dev@b7f56ef" {
		t.Fatalf("dev: %+v, %v", res, err)
	}
	_, err = resolve("v1")
	if err == nil || !strings.Contains(err.Error(), `no branch "v1"`) || !strings.Contains(err.Error(), "dev, master") {
		t.Errorf("missing branch: %v", err)
	}

	// A server with no symref: HEAD's hash names the branch when only one has it.
	srv2 := gitServer(t, pkt("# service=git-upload-pack\n", "", shaDev+" HEAD\x00thin-pack\n", shaDev+" refs/heads/trunk\n", shaMaster+" refs/heads/old\n", ""), 0)
	app := &def.App{ID: "r", Source: def.Source{Type: def.SourceGit, URL: srv2.URL + "/o/r/"}}
	if res, err := (Git{}).Resolve(context.Background(), client("", ""), app, nil); err != nil || res.Display != "trunk@b7f56ef" {
		t.Errorf("no symref: %+v, %v", res, err)
	}

	// Dumb HTTP serves the refs file as is.
	dumb := gitServer(t, shaMaster+"\trefs/heads/main\n"+shaTag+"\trefs/tags/v1\n", 0)
	app.Source.URL = dumb.URL + "/o/r"
	if _, err := (Git{}).Resolve(context.Background(), client("", ""), app, nil); err == nil || !strings.Contains(err.Error(), "set source.branch") {
		t.Errorf("dumb server without a branch: %v", err)
	}
	app.Source.Branch = "main"
	if res, err := (Git{}).Resolve(context.Background(), client("", ""), app, nil); err != nil || res.Version != shaMaster {
		t.Errorf("dumb server: %+v, %v", res, err)
	}

	gone := gitServer(t, "", http.StatusNotFound)
	app.Source.URL = gone.URL + "/o/r"
	if _, err := (Git{}).Resolve(context.Background(), client("", ""), app, nil); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("404: %v", err)
	}
}
