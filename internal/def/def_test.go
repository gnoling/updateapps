package def

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var vars = Vars{AppDir: "/apps", AppImageDir: "/apps/appimages", Home: "/home/u"}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	p := write(t, t.TempDir(), "rmg.yaml", "source: {type: github-release, repo: Rosalie241/RMG}\n")
	a, err := LoadFile(p, vars)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "rmg" || a.Name != "rmg" || !a.IsEnabled() {
		t.Errorf("id/name/enabled = %q %q %v", a.ID, a.Name, a.IsEnabled())
	}
	if a.Asset.Glob != DefaultAssetGlob || a.Asset.Pick != "first" {
		t.Errorf("asset defaults = %+v", a.Asset)
	}
	if a.Source.Channel != "latest" || a.Source.VersionFrom != "release" {
		t.Errorf("source defaults = %+v", a.Source)
	}
	if a.Install.Type != InstallFile || a.Install.Dest != "/apps/appimages/rmg" {
		t.Errorf("install defaults = %+v", a.Install)
	}
}

func TestAssetStringOrMap(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadFile(write(t, dir, "a.yaml", "source: {type: github-release, repo: o/r}\nasset: '*.zip'\n"), vars)
	if err != nil || a.Asset.Glob != "*.zip" {
		t.Fatalf("string form: %v %+v", err, a)
	}
	b, err := LoadFile(write(t, dir, "b.yaml", "source: {type: github-release, repo: o/r}\nasset: {regex: '^x$', exclude_regex: 'sig$', pick: last}\n"), vars)
	if err != nil || b.Asset.Regex != "^x$" || b.Asset.Pick != "last" || b.Asset.Glob != "" {
		t.Fatalf("map form: %v %+v", err, b.Asset)
	}
}

func TestDestForms(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadFile(write(t, dir, "tool.yaml", "source: {type: github-release, repo: o/r}\ninstall: {type: file, dest: '${APPDIR}/tools/'}\n"), vars)
	if err != nil || a.Install.Dest != "/apps/tools/tool" {
		t.Fatalf("trailing slash: %v %q", err, a.Install.Dest)
	}
	b, err := LoadFile(write(t, dir, "h.yaml", "source: {type: github-release, repo: o/r}\ninstall: {type: extract, dest: '~/games/${name}'}\n"), vars)
	if err != nil || b.Install.Dest != "/home/u/games/h" {
		t.Fatalf("tilde: %v %q", err, b.Install.Dest)
	}
}

func TestTagImpliesChannel(t *testing.T) {
	a, err := LoadFile(write(t, t.TempDir(), "d.yaml", "source: {type: github-release, repo: o/r, tag: continuous}\n"), vars)
	if err != nil || a.Source.Channel != "tag" {
		t.Fatalf("%v %+v", err, a)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"typo is an error":             {"source: {type: github-release, repo: o/r}\ndescripton: x\n", "descripton"},
		"typo in asset map":            {"source: {type: github-release, repo: o/r}\nasset: {globb: x}\n", "globb"},
		"missing type":                 {"name: x\n", "source.type is required"},
		"unknown type":                 {"source: {type: ftp}\n", "not a known source type"},
		"missing required":             {"source: {type: github-release}\n", "source.repo is required"},
		"field of another type":        {"source: {type: github-release, repo: o/r, workflow: ci}\n", "source.workflow does not apply"},
		"one-of both set":              {"source: {type: github-actions, repo: o/r, workflow: ci, artifact: a, artifact_regex: b}\n", "only one of"},
		"bad channel":                  {"source: {type: github-release, repo: o/r, channel: nightly}\n", "source.channel"},
		"channel tag sans tag":         {"source: {type: github-release, repo: o/r, channel: tag}\n", "go together"},
		"bad version_from":             {"source: {type: github-release, repo: o/r, version_from: x}\n", "version_from"},
		"bad regex":                    {"source: {type: github-release, repo: o/r}\nasset: {regex: '('}\n", "asset.regex"},
		"bad glob":                     {"source: {type: github-release, repo: o/r}\nasset: '[a'\n", "bad glob"},
		"glob and regex":               {"source: {type: github-release, repo: o/r}\nasset: {glob: a, regex: b}\n", "not both"},
		"unknown install":              {"source: {type: github-release, repo: o/r}\ninstall: {type: copy}\n", "not a known install type"},
		"extract needs dest":           {"source: {type: github-release, repo: o/r}\ninstall: {type: extract}\n", "install.dest is required"},
		"strip on file install":        {"source: {type: github-release, repo: o/r}\ninstall: {type: file, strip: 1}\n", "install.strip does not apply"},
		"unknown variable":             {"source: {type: github-release, repo: o/r}\ninstall: {type: extract, dest: '${APPDR}/x'}\n", "unknown variable"},
		"asset on etag source":         {"source: {type: http-etag, url: 'http://x'}\nasset: x\n", "asset does not apply"},
		"html version keyword":         {"source: {type: html, url: 'http://x', select: a, version: newest}\n", "basename, full or regex"},
		"html version groups":          {"source: {type: html, url: 'http://x', select: a, version: 'regex:v[0-9]+'}\n", "one capture group"},
		"bad css selector":             {"source: {type: html, url: 'http://x', select: 'a[[['}\n", "source.select"},
		"missing script file":          {"source: {type: script, script_file: nope.lua}\n", "script_file"},
		"bad download template":        {"source: {type: html, url: 'http://x', select: a, download: '{{.Value'}\n", "source.download"},
		"flatpak source, file install": {"source: {type: flatpak, ref: 'https://x/a.flatpakref'}\ninstall: {type: file, dest: /x}\n", "needs install type flatpak"},
		"flatpak app without remote":   {"source: {type: flatpak, app: org.x.Y}\n", "source.remote is required"},
		"bad flatpak scope":            {"source: {type: flatpak, ref: 'https://x/a.flatpakref'}\ninstall: {type: flatpak, scope: root}\n", "install.scope"},
		"dest on a deb":                {"source: {type: github-release, repo: o/r}\ninstall: {type: deb, dest: /x}\n", "install.dest does not apply"},
		"none without post":            {"source: {type: http-etag, url: 'http://x'}\ninstall: {type: none}\n", "does nothing without post"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFile(write(t, t.TempDir(), "x.yaml", tc.yaml), vars)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// Later milestones' source and install types must already validate, so
// converted definitions can be checked before their resolvers exist.
func TestFutureTypesValidate(t *testing.T) {
	cases := map[string]string{
		"actions": "source: {type: github-actions, repo: o/r, workflow: Build, artifact: x}\ninstall: {type: extract-one, member: 'bin/x', dest: '${APPDIR}/x/x'}\n",
		"forgejo": "source: {type: forgejo, host: git.example, repo: o/r}\nasset: {regex: 'AppImage$'}\n",
		"gitlab":  "source: {type: gitlab, project: a/b}\nasset: {regex: 'x$'}\n",
		"html":    "source: {type: html, url: 'https://x', select: 'a', attr: href, pick: first}\ninstall: {type: extract, dest: '${APPDIR}/x', strip: 1, exclude: ['*.json']}\n",
		"etag":    "source: {type: http-etag, url: 'https://x', headers: {User-Agent: curl/8}}\n",
		"yaml":    "source: {type: yaml, url: 'https://x', version: '.v', download: 'https://x/{{.Version}}'}\n",
		"script":  "source: {type: script, lua: 'return {}'}\ninstall: {type: none}\npost: ['echo hi']\n",
		"deb":     "source: {type: github-release, repo: o/r}\nasset: '*_amd64.deb'\ninstall: {type: deb, package: bat}\n",
		"bundle":  "source: {type: github-release, repo: o/r}\nasset: '*.flatpak'\ninstall: {type: flatpak, scope: system}\n",
		"fpref":   "source: {type: flatpak, ref: 'https://flatpak.example/dev.flatpakref'}\n",
		"fpapp":   "source: {type: flatpak, remote: flathub, app: org.libretro.RetroArch, branch: stable}\n",
	}
	for name, body := range cases {
		if _, err := LoadFile(write(t, t.TempDir(), name+".yaml", body), vars); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoadDirKeepsGoodFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.yaml", "source: {type: github-release, repo: o/r}\n")
	write(t, dir, "bad.yaml", "source: {type: nope}\n")
	write(t, dir, "README.md", "ignored")
	apps, errs := LoadDir(dir, vars)
	if len(apps) != 1 || apps[0].ID != "good" || len(errs) != 1 {
		t.Fatalf("apps=%v errs=%v", apps, errs)
	}
}

func TestExamplesValidate(t *testing.T) {
	apps, errs := LoadDir("../../examples/apps.d", vars)
	if len(errs) > 0 || len(apps) == 0 {
		t.Fatalf("apps=%d errs=%v", len(apps), errs)
	}
}

func TestSelect(t *testing.T) {
	off := false
	apps := []*App{
		{ID: "gearboy", Name: "gearboy", Source: Source{Repo: "drhelius/Gearboy"}},
		{ID: "gearlynx", Name: "gearlynx", Source: Source{Repo: "drhelius/Gearlynx"}},
		{ID: "krita", Name: "Krita", Source: Source{URL: "https://krita.org/en/download/"}},
		{ID: "ryujinx", Name: "Ryujinx.AppImage", Enabled: &off, Source: Source{Repo: "Ryubing/Ryujinx"}},
	}
	ids := func(filters ...string) string {
		var out []string
		for _, a := range Select(apps, filters) {
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	for want, filters := range map[string][]string{
		"gearboy,gearlynx,krita": nil,                      // no filter: enabled only
		"gearboy,gearlynx":       {"GEAR"},                 // case-insensitive substring
		"gearboy,krita":          {"gearboy", "krita.org"}, // OR'd; matches URL
		"gearlynx":               {"drhelius/gearl"},       // matches repo
		"ryujinx":                {"Ryujinx.AppImage"},     // disabled runs when named exactly
		"":                       {"ryu"},                  // ...but not on a substring
	} {
		if got := ids(filters...); got != want {
			t.Errorf("Select(%v) = %q, want %q", filters, got, want)
		}
	}
}

func TestForgejoDefaultAsset(t *testing.T) {
	a, err := LoadFile(write(t, t.TempDir(), "f.yaml", "source: {type: forgejo, host: git.example, repo: o/r}\n"), vars)
	if err != nil || a.Asset.Regex != DefaultForgejoAssetRegex {
		t.Fatalf("%v %+v", err, a)
	}
}
