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

var defaultLine = regexp.MustCompile(`^(\s*default:\s*)(\S+)(.*)$`)

// SetRepoDefault sets a repository's default: to "enabled" or "disabled". If
// the config has no repositories: key (the built-in default is in effect),
// that repository is written out first.
func (e *Editor) SetRepoDefault(name, value string) error {
	top, err := e.top()
	if err != nil {
		return err
	}
	_, repos := lookup(top, "repositories")
	if repos == nil {
		if name != DefaultRepository.Name {
			return fmt.Errorf("no repository named %s", name)
		}
		e.appendBlock("repositories:", "  - name: "+DefaultRepository.Name, "    url: "+DefaultRepository.URL, "    default: "+value)
		return nil
	}
	for _, item := range repos.Content {
		if _, n := lookup(item, "name"); n == nil || n.Value != name {
			continue
		}
		if item.Style&yaml.FlowStyle != 0 {
			return fmt.Errorf("%s: can't edit repository %s written as {...}; write it one key per line", e.path, name)
		}
		if k, v := lookup(item, "default"); k != nil {
			m := defaultLine.FindStringSubmatch(e.lines[v.Line-1])
			if m == nil {
				return fmt.Errorf("%s:%d: can't edit this default: line", e.path, v.Line)
			}
			e.replace(v.Line, m[1]+value+m[3])
			return nil
		}
		// After the item's last key, indented like its first.
		lastKey := item.Content[len(item.Content)-2]
		e.insertAfter(lastKey.Line, strings.Repeat(" ", item.Content[0].Column-1)+"default: "+value)
		return nil
	}
	return fmt.Errorf("no repository named %s", name)
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
