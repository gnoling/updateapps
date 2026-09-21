package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T) {
	for _, k := range []string{"APPDIR", "APPIMAGEDIR", "MAXJOBS", "FORCE", "VERBOSE", "XDG_STATE_HOME"} {
		t.Setenv(k, "")
	}
}

func TestDefaultsWithoutAFile(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "/home/u")
	c, err := Load(filepath.Join(t.TempDir(), "nope", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppDir != "/home/u/apps" || c.AppImageDir != "/home/u/apps/appimages" || c.Jobs != 12 {
		t.Errorf("%+v", c)
	}
	if c.Definitions != filepath.Join(filepath.Dir(c.Path), "apps.d") {
		t.Errorf("definitions = %s", c.Definitions)
	}
}

func TestFileThenEnvPrecedence(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "/home/u")
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("appdir: ~/software\njobs: 4\nstate: ~/s.json\n"), 0o644)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AppDir != "/home/u/software" || c.AppImageDir != "/home/u/software/appimages" || c.Jobs != 4 || c.State != "/home/u/s.json" {
		t.Errorf("file: %+v", c)
	}

	t.Setenv("APPDIR", "/mnt/apps")
	t.Setenv("MAXJOBS", "2")
	t.Setenv("FORCE", "1")
	t.Setenv("VERBOSE", "2")
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AppDir != "/mnt/apps" || c.AppImageDir != "/mnt/apps/appimages" || c.Jobs != 2 || !c.Force || c.Verbose != 2 {
		t.Errorf("env: %+v", c)
	}
}

func TestBadConfig(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("appdirr: /x\n"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("a typo'd key should be an error")
	}
	os.WriteFile(path, []byte(""), 0o644)
	if _, err := Load(path); err != nil {
		t.Errorf("an empty config file is fine: %v", err)
	}
	t.Setenv("MAXJOBS", "lots")
	if _, err := Load(path); err == nil {
		t.Error("MAXJOBS=lots should be an error")
	}
}

func TestRepositories(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "/home/u")
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(body string) { os.WriteFile(path, []byte(body), 0o644) }

	write("repositories:\n  - {name: main, url: 'https://github.com/o/defs'}\n  - {name: local, path: '~/defs', trusted: true}\nenabled: [main/x]\ndisabled: [y]\nauto_pull: true\n")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repositories) != 2 || c.Repositories[1].Path != "/home/u/defs" || c.Repositories[0].Trusted || !c.Repositories[1].Trusted {
		t.Errorf("%+v", c.Repositories)
	}
	// Repositories live in folders under definitions:, which defaults to apps.d beside the config.
	if c.Definitions != filepath.Join(filepath.Dir(path), "apps.d") || !c.AutoPull || c.Enabled[0] != "main/x" {
		t.Errorf("definitions=%q autopull=%v enabled=%v", c.Definitions, c.AutoPull, c.Enabled)
	}

	// Neither url nor path: a folder of the user's own at apps.d/<name>/.
	write("repositories: [{name: mine, trusted: true}]\n")
	if c, err = Load(path); err != nil || c.Repositories[0].Path != "" {
		t.Errorf("name-only repository: %v %+v", err, c)
	}

	for name, body := range map[string]string{
		"both url and path": "repositories: [{name: a, url: 'https://x/o/r', path: /x}]\n",
		"no name":           "repositories: [{url: 'https://x/o/r'}]\n",
		"name with slash":   "repositories: [{name: a/b, path: /x}]\n",
		"duplicate names":   "repositories: [{name: a, path: /x}, {name: a, path: /y}]\n",
		"bad default":       "repositories: [{name: a, path: /x, default: off}]\n",
	} {
		write(body)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDefaultRepository(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	// No repositories: key: the public set, opt-in so nothing installs unasked.
	c, err := Load(path)
	if err != nil || len(c.Repositories) != 1 || c.Repositories[0] != DefaultRepository || c.Repositories[0].Default != "disabled" || c.Repositories[0].Trusted {
		t.Fatalf("err=%v repos=%+v", err, c.Repositories)
	}
	// An explicit empty list means none.
	os.WriteFile(path, []byte("repositories: []\n"), 0o644)
	if c, err = Load(path); err != nil || len(c.Repositories) != 0 {
		t.Fatalf("err=%v repos=%+v", err, c.Repositories)
	}
}

// The documented sample must load, and must spell out the real defaults.
func TestSampleConfigLoads(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", "/home/u")
	sample, err := Load("../../examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defaults, _ := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if sample.AppDir != defaults.AppDir || sample.AppImageDir != defaults.AppImageDir || sample.Jobs != defaults.Jobs ||
		sample.Privilege != defaults.Privilege || sample.FlatpakScope != defaults.FlatpakScope || sample.AutoPull != defaults.AutoPull ||
		len(sample.Repositories) != 1 || sample.Repositories[0] != DefaultRepository {
		t.Errorf("examples/config.yaml doesn't match the built-in defaults:\nsample   %+v\ndefaults %+v", sample, defaults)
	}
}
