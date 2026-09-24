package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Editor changes a config file in place for the CLI and GUI. It rewrites only
// the lines it must: re-encoding the YAML would drop the blank lines and
// comment alignment of a hand-maintained file.
type Editor struct {
	path  string
	lines []string
}

// OpenEditor reads path; a missing file is an empty config.
func OpenEditor(path string) (*Editor, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	e := &Editor{path: path}
	if text := strings.TrimRight(string(data), "\n"); text != "" {
		e.lines = strings.Split(text, "\n")
	}
	return e, nil
}

// top returns the document's top-level mapping, or nil if there's none yet.
func (e *Editor) top() (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(e.lines, "\n")), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", e.path, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a mapping", e.path)
	}
	return doc.Content[0], nil
}

func lookup(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

func (e *Editor) replace(line int, with ...string) { // line is 1-based
	e.lines = append(e.lines[:line-1], append(with, e.lines[line:]...)...)
}

func (e *Editor) insertAfter(line int, with ...string) {
	e.lines = append(e.lines[:line], append(with, e.lines[line:]...)...)
}

func (e *Editor) appendBlock(block ...string) {
	if len(e.lines) > 0 && e.lines[len(e.lines)-1] != "" {
		e.lines = append(e.lines, "")
	}
	e.lines = append(e.lines, block...)
}

var (
	flowSeq = regexp.MustCompile(`^(\s*[\w-]+:\s*)\[(.*)\](.*)$`)
	bareKey = regexp.MustCompile(`^(\s*[\w-]+:)(\s*)(#.*)?$`)
)

// SetListed adds value to, or removes it from, the top-level list key
// (enabled / disabled). Other entries and their comments are left alone.
func (e *Editor) SetListed(key, value string, present bool) error {
	top, err := e.top()
	if err != nil {
		return err
	}
	k, v := lookup(top, key)
	if k == nil {
		if present {
			e.appendBlock(key+":", "  - "+value)
		}
		return nil
	}
	if v.Kind != yaml.SequenceNode {
		if v.Tag == "!!null" && present { // "enabled:" with nothing after it
			e.insertAfter(k.Line, "  - "+value)
			return nil
		}
		if v.Tag == "!!null" {
			return nil
		}
		return fmt.Errorf("%s: %s isn't a list", e.path, key)
	}
	at := -1
	for i, item := range v.Content {
		if item.Value == value {
			at = i
		}
	}
	if (at >= 0) == present {
		return nil
	}

	if m := flowSeq.FindStringSubmatch(e.lines[k.Line-1]); v.Style&yaml.FlowStyle != 0 && m != nil {
		var items []string
		for i, item := range v.Content {
			if i != at {
				items = append(items, item.Value)
			}
		}
		if present && len(items) == 0 {
			// Grow an empty [] as a block list: one id per line stays readable.
			e.replace(k.Line, strings.TrimRight(strings.TrimRight(m[1], " ")+m[3], " "), "  - "+value)
			return nil
		}
		if present {
			items = append(items, value)
		}
		e.replace(k.Line, m[1]+"["+strings.Join(items, ", ")+"]"+m[3])
		return nil
	}
	if v.Style&yaml.FlowStyle != 0 {
		return fmt.Errorf("%s: can't edit the multi-line list %s; write it one item per line", e.path, key)
	}
	if !present {
		if len(v.Content) == 1 { // removing the last item leaves "key: []"
			e.replace(v.Content[0].Line)
			if m := bareKey.FindStringSubmatch(e.lines[k.Line-1]); m != nil {
				e.replace(k.Line, strings.TrimRight(m[1]+" []"+m[2]+m[3], " "))
			}
		} else {
			e.replace(v.Content[at].Line)
		}
		return nil
	}
	last := v.Content[len(v.Content)-1]
	e.insertAfter(last.Line, strings.Repeat(" ", last.Column-3)+"- "+value)
	return nil
}

// scalarLine splits "key: value   # comment"; a comment needs whitespace
// before its #, so a # inside a value survives.
var scalarLine = regexp.MustCompile(`^(\s*[\w-]+:\s*)(\S.*?)?(\s+#.*)?$`)

// encode renders a value as one YAML scalar, quoted when it must be.
func encode(value any) (string, error) {
	out, err := yaml.Marshal(value)
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(out), "\n")
	if strings.Contains(s, "\n") {
		return "", fmt.Errorf("%q doesn't fit on one line", value)
	}
	return s, nil
}

// setValueLine rewrites the value on line (1-based), keeping its comment.
func (e *Editor) setValueLine(line int, value any) error {
	m := scalarLine.FindStringSubmatch(e.lines[line-1])
	if m == nil {
		return fmt.Errorf("%s:%d: can't edit this line", e.path, line)
	}
	s, err := encode(value)
	if err != nil {
		return err
	}
	e.replace(line, strings.TrimRight(m[1], " ")+" "+s+m[3])
	return nil
}

// SetScalar sets a top-level scalar key (appdir, jobs, github_token...). A nil
// value removes it. A commented-out "# key: ..." line is uncommented and
// reused, so its explanation stays beside the value.
func (e *Editor) SetScalar(key string, value any) error {
	top, err := e.top()
	if err != nil {
		return err
	}
	k, v := lookup(top, key)
	switch {
	case k != nil && !oneLine(k, v):
		return fmt.Errorf("%s: %s isn't a one-line value", e.path, key)
	case k != nil && value == nil:
		e.replace(k.Line)
		return nil
	case k != nil:
		return e.setValueLine(k.Line, value)
	case value == nil:
		return nil
	}
	commented := regexp.MustCompile(`^#\s*` + regexp.QuoteMeta(key) + `:(\s|$)`)
	for i, line := range e.lines {
		if commented.MatchString(line) {
			e.lines[i] = strings.TrimLeft(line[1:], " ")
			return e.setValueLine(i+1, value)
		}
	}
	s, err := encode(value)
	if err != nil {
		return err
	}
	e.lines = append(e.lines, key+": "+s)
	return nil
}

// oneLine reports whether v is a scalar written on k's line.
func oneLine(k, v *yaml.Node) bool {
	return v.Kind == yaml.ScalarNode && v.Line == k.Line && v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0
}

// SetApp turns one app on or off by name in the enabled:/disabled: lists.
// Always listed, even where a default would do: a choice made by name has to
// survive a later enable/disable --all.
func (e *Editor) SetApp(id, repo string, on bool) error {
	list, other := "enabled", "disabled"
	if !on {
		list, other = other, list
	}
	for _, entry := range []string{id, repo + "/" + id} {
		if err := e.SetListed(other, entry, false); err != nil {
			return err
		}
	}
	return e.SetListed(list, id, true)
}

// repositories returns the repositories: sequence, writing the built-in
// default out first if the file has no such key (it's in effect implicitly,
// and would be lost by adding a key without it).
func (e *Editor) repositories() (key, repos *yaml.Node, err error) {
	top, err := e.top()
	if err != nil {
		return nil, nil, err
	}
	if key, repos = lookup(top, "repositories"); repos != nil {
		return key, repos, nil
	}
	e.appendBlock("repositories:", "  - name: "+DefaultRepository.Name, "    url: "+DefaultRepository.URL, "    default: "+DefaultRepository.Default)
	if top, err = e.top(); err != nil {
		return nil, nil, err
	}
	key, repos = lookup(top, "repositories")
	return key, repos, nil
}

func (e *Editor) repository(name string) (*yaml.Node, error) {
	_, repos, err := e.repositories()
	if err != nil {
		return nil, err
	}
	for _, item := range repos.Content {
		if _, n := lookup(item, "name"); n != nil && n.Value == name {
			if item.Style&yaml.FlowStyle != 0 {
				return nil, fmt.Errorf("%s: can't edit repository %s written as {...}; write it one key per line", e.path, name)
			}
			return item, nil
		}
	}
	return nil, fmt.Errorf("no repository named %s", name)
}

// lastLine is the last line a node occupies.
func lastLine(n *yaml.Node) int {
	last := n.Line
	for _, c := range n.Content {
		if l := lastLine(c); l > last {
			last = l
		}
	}
	return last
}

// SetRepoField sets one key of a repository (default, trusted, branch...); a
// nil value removes it.
func (e *Editor) SetRepoField(name, key string, value any) error {
	if key == "name" {
		return errors.New("a repository can't be renamed: its folder and ids carry the name")
	}
	item, err := e.repository(name)
	if err != nil {
		return err
	}
	k, v := lookup(item, key)
	switch {
	case k != nil && !oneLine(k, v):
		return fmt.Errorf("%s:%d: %s isn't a one-line value", e.path, k.Line, key)
	case k != nil && value == nil:
		e.replace(k.Line)
		return nil
	case k != nil:
		return e.setValueLine(k.Line, value)
	case value == nil:
		return nil
	}
	s, err := encode(value)
	if err != nil {
		return err
	}
	// After the item's last line, indented like its first key.
	e.insertAfter(lastLine(item), strings.Repeat(" ", item.Content[0].Column-1)+key+": "+s)
	return nil
}

// SetRepoDefault sets a repository's default: to "enabled" or "disabled".
func (e *Editor) SetRepoDefault(name, value string) error {
	return e.SetRepoField(name, "default", value)
}

// AddRepository appends one to repositories:, after the last.
func (e *Editor) AddRepository(r Repository) error {
	key, repos, err := e.repositories()
	if err != nil {
		return err
	}
	for _, item := range repos.Content {
		if _, n := lookup(item, "name"); n != nil && n.Value == r.Name {
			return fmt.Errorf("a repository named %s already exists", r.Name)
		}
	}
	block := []string{"- name: " + r.Name}
	for _, f := range []struct {
		key   string
		value any
		set   bool
	}{
		{"url", r.URL, r.URL != ""}, {"path", r.Path, r.Path != ""}, {"branch", r.Branch, r.Branch != ""},
		{"default", r.Default, r.Default != ""}, {"trusted", r.Trusted, r.Trusted},
	} {
		if f.set {
			s, err := encode(f.value)
			if err != nil {
				return err
			}
			block = append(block, "  "+f.key+": "+s)
		}
	}
	if repos.Kind != yaml.SequenceNode || (repos.Style&yaml.FlowStyle != 0 && len(repos.Content) > 0) {
		return fmt.Errorf("%s: can't add to repositories written as [...]; write it one entry per line", e.path)
	}
	indent, after := "  ", key.Line
	if len(repos.Content) > 0 {
		first, last := repos.Content[0], repos.Content[len(repos.Content)-1]
		indent, after = strings.Repeat(" ", first.Column-3), lastLine(last)
	} else {
		// "repositories: []" becomes a block list.
		m := flowSeq.FindStringSubmatch(e.lines[key.Line-1])
		if m == nil {
			return fmt.Errorf("%s:%d: can't edit this repositories: line", e.path, key.Line)
		}
		e.replace(key.Line, strings.TrimRight(strings.TrimRight(m[1], " ")+m[3], " "))
	}
	for i := range block {
		block[i] = indent + block[i]
	}
	e.insertAfter(after, block...)
	return nil
}

// RemoveRepository deletes a repository's entry. Removing the last one leaves
// "repositories: []": a bare key would bring the built-in default back.
func (e *Editor) RemoveRepository(name string) error {
	item, err := e.repository(name)
	if err != nil {
		return err
	}
	key, repos, _ := e.repositories()
	from, to := item.Line, lastLine(item)
	e.lines = append(e.lines[:from-1], e.lines[to:]...)
	if len(repos.Content) == 1 {
		if m := bareKey.FindStringSubmatch(e.lines[key.Line-1]); m != nil {
			e.replace(key.Line, strings.TrimRight(m[1]+" []"+m[2]+m[3], " "))
		}
	}
	return nil
}

// Save writes the file, but only if the result still loads.
func (e *Editor) Save() error {
	if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.path), ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(e.lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := Load(tmp.Name()); err != nil {
		return fmt.Errorf("refusing to write %s: the edit would break it: %w", e.path, err)
	}
	if info, err := os.Stat(e.path); err == nil {
		os.Chmod(tmp.Name(), info.Mode().Perm())
	} else {
		os.Chmod(tmp.Name(), 0o644)
	}
	return os.Rename(tmp.Name(), e.path)
}
