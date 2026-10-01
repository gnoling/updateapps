package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/gnoling/updateapps/internal/def"
	"github.com/gnoling/updateapps/internal/fetch"
)

// Git resolves a branch head from the repository's ref advertisement, the
// first exchange of a clone over smart HTTP. One GET, no git binary: checks
// stay cheap and work anywhere. The version is the commit hash.
type Git struct{}

func (Git) Resolve(ctx context.Context, c *fetch.Client, app *def.App, _ *Cache) (*Result, error) {
	base := strings.TrimSuffix(app.Source.URL, "/")
	resp, err := c.Do(ctx, "GET", base+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		var se *fetch.StatusError
		if errors.As(err, &se) && (se.Code == 401 || se.Code == 404) {
			return nil, fmt.Errorf("%s: %s (no such repository, or it's private)", base, se.Status)
		}
		return nil, err
	}
	defer resp.Body.Close()
	adv, err := parseRefs(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", base, err)
	}
	branch := app.Source.Branch
	if branch == "" {
		branch = adv.head
	}
	if branch == "" {
		return nil, errors.New("the server doesn't say which branch HEAD is; set source.branch")
	}
	sha, ok := adv.refs["refs/heads/"+branch]
	if !ok {
		var names []string
		for ref := range adv.refs {
			if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
				names = append(names, b)
			}
		}
		sort.Strings(names)
		if len(names) > 20 {
			names = append(names[:20], fmt.Sprintf("... and %d more", len(names)-20))
		}
		return nil, fmt.Errorf("no branch %q; the repository has: %s", branch, strings.Join(names, ", "))
	}
	return &Result{Version: sha, Display: branch + "@" + sha[:7]}, nil
}

type refAdvertisement struct {
	refs map[string]string // ref name -> hash
	head string            // branch HEAD points at, if advertised
}

// parseRefs reads a smart-HTTP advertisement (pkt-lines), or the plain
// "hash\tref" file a dumb server returns.
func parseRefs(r io.Reader) (*refAdvertisement, error) {
	br := bufio.NewReader(r)
	adv := &refAdvertisement{refs: map[string]string{}}
	peek, err := br.Peek(41)
	if err != nil {
		return nil, errors.New("empty response; not a git repository?")
	}
	if peek[40] == '\t' {
		// Dumb HTTP: one "hash<TAB>ref" per line.
		sc := bufio.NewScanner(br)
		for sc.Scan() {
			hash, ref, ok := strings.Cut(sc.Text(), "\t")
			if ok && len(hash) >= 40 {
				adv.refs[ref] = hash
			}
		}
		if len(adv.refs) == 0 {
			return nil, errors.New("not a git ref advertisement")
		}
		return adv, nil
	}
	first := true
	for {
		line, err := pktLine(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if line == nil { // flush
			continue
		}
		text := strings.TrimSuffix(string(line), "\n")
		if first {
			first = false
			if strings.HasPrefix(text, "# service=") {
				continue
			}
		}
		if strings.HasPrefix(text, "ERR ") {
			return nil, errors.New(strings.TrimPrefix(text, "ERR "))
		}
		entry, caps, _ := strings.Cut(text, "\x00")
		for _, cap := range strings.Fields(caps) {
			if target, ok := strings.CutPrefix(cap, "symref=HEAD:refs/heads/"); ok {
				adv.head = target
			}
		}
		hash, ref, ok := strings.Cut(entry, " ")
		if !ok || len(hash) < 40 || strings.HasSuffix(ref, "^{}") {
			continue
		}
		adv.refs[ref] = hash
	}
	if adv.head == "" {
		// Protocol v2 servers and some v0 ones omit the symref: a branch at
		// HEAD's hash is the best guess, if there's exactly one.
		var at []string
		for ref, hash := range adv.refs {
			if hash == adv.refs["HEAD"] && strings.HasPrefix(ref, "refs/heads/") {
				at = append(at, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		if len(at) == 1 {
			adv.head = at[0]
		}
	}
	if len(adv.refs) == 0 {
		return nil, errors.New("the repository has no refs")
	}
	return adv, nil
}

// pktLine reads one pkt-line: a 4-digit hex length including itself, then
// the payload. A flush packet (0000) returns nil, nil.
func pktLine(r *bufio.Reader) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}
	n, err := strconv.ParseUint(string(head[:]), 16, 16)
	if err != nil {
		return nil, fmt.Errorf("bad pkt-line length %q", head)
	}
	if n == 0 {
		return nil, nil
	}
	if n < 4 {
		return nil, fmt.Errorf("bad pkt-line length %d", n)
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("truncated pkt-line: %v", err)
	}
	return buf, nil
}
