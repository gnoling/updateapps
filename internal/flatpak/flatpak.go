// Package flatpak wraps the flatpak CLI; there is no CGO-free libflatpak.
package flatpak

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Ref identifies an app build: org.example.App//stable.
type Ref struct {
	ID, Branch string
	Remote     string // where it comes from, when known
	URL        string // the repo URL from a .flatpakref
}

func (r Ref) String() string {
	if r.Branch == "" {
		return r.ID // any branch; fine when only one is installed
	}
	return r.ID + "//" + r.Branch
}

// ParseRef reads a .flatpakref (an INI file with one [Flatpak Ref] group).
func ParseRef(data []byte) (Ref, error) {
	var r Ref
	inGroup := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inGroup = line == "[Flatpak Ref]"
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !inGroup || !ok {
			continue
		}
		switch value = strings.TrimSpace(value); strings.TrimSpace(key) {
		case "Name":
			r.ID = value
		case "Branch":
			r.Branch = value
		case "Url":
			r.URL = value
		case "SuggestRemoteName":
			r.Remote = value
		}
	}
	if r.ID == "" {
		return r, errors.New("not a .flatpakref: no Name in a [Flatpak Ref] group")
	}
	if r.Branch == "" {
		r.Branch = "stable"
	}
	return r, nil
}

// IsRef reports whether data looks like a .flatpakref rather than a bundle.
func IsRef(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(data), []byte("[Flatpak Ref]"))
}

// ScopeFlag is --user or --system.
func ScopeFlag(user bool) string {
	if user {
		return "--user"
	}
	return "--system"
}

// Available reports whether the flatpak CLI is installed.
func Available() error {
	if _, err := exec.LookPath("flatpak"); err != nil {
		return errors.New("flatpak isn't installed")
	}
	return nil
}

func run(ctx context.Context, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "flatpak", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return "", fmt.Errorf("flatpak %s: %s", args[0], strings.TrimPrefix(msg, "error: "))
	}
	return strings.TrimSpace(string(out)), nil
}

// Installed returns the installed commit and origin remote of ref, or
// ok=false if it isn't installed in that scope.
func Installed(ctx context.Context, user bool, ref Ref) (commit, origin string, ok bool) {
	commit, err := run(ctx, "info", ScopeFlag(user), "--show-commit", ref.String())
	if err != nil || commit == "" {
		return "", "", false
	}
	origin, _ = run(ctx, "info", ScopeFlag(user), "--show-origin", ref.String())
	return commit, origin, true
}

// Remote is what a remote currently offers for a ref.
type Remote struct{ Commit, Version string }

// RemoteInfo asks remote (over the network) what it has for ref.
func RemoteInfo(ctx context.Context, user bool, remote string, ref Ref) (Remote, error) {
	out, err := run(ctx, "remote-info", ScopeFlag(user), remote, ref.String())
	if err != nil {
		return Remote{}, err
	}
	var r Remote
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch value = strings.TrimSpace(value); strings.TrimSpace(key) {
		case "Commit":
			r.Commit = value
		case "Version":
			r.Version = value
		}
	}
	if r.Commit == "" {
		return r, fmt.Errorf("flatpak remote-info %s %s reported no commit", remote, ref)
	}
	return r, nil
}

// Display is the app's version, if it publishes one, plus the short commit
// that identifies the build.
func Display(version, commit string) string {
	if len(commit) > 10 {
		commit = commit[:10]
	}
	if version == "" {
		return commit
	}
	return version + " (" + commit + ")"
}
