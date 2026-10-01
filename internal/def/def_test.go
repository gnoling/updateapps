package def

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"git needs build":              {"source: {type: git, url: 'https://x/r'}\ninstall: {type: extract, dest: '${APPDIR}/x'}\n", "needs install type build"},
		"build needs git":              {"source: {type: github-release, repo: o/r}\ninstall: {type: build, dest: '${APPDIR}/x', steps: [make], artifacts: [x]}\n", "needs source type git"},
		"git needs https":              {"source: {type: git, url: 'git@x:r.git'}\ninstall: {dest: '${APPDIR}/x', steps: [make], artifacts: [x]}\n", "source.url must be an http(s) URL"},
		"build needs steps":            {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', artifacts: [x]}\n", "install.steps is required"},
		"build needs artifacts":        {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', steps: [make]}\n", "install.artifacts is required"},
		"artifact escapes":             {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', steps: [make], artifacts: ['../x']}\n", "inside the work tree"},
		"artifact to glob":             {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', steps: [make], artifacts: [{from: x, to: '*.y'}]}\n", "plain path inside dest"},
		"artifact typo":                {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', steps: [make], artifacts: [{form: x}]}\n", "form"},
		"bad need name":                {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', steps: [make], artifacts: [x], needs: {apt: ['lib; rm -rf /']}}\n", "install.needs.apt"},
		"bad build timeout":            {"source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/x', steps: [make], artifacts: [x], timeout: soon}\n", "install.timeout"},
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
		"build":   "source: {type: git, url: 'https://github.com/o/r', branch: dev, submodules: true}\ninstall: {dest: '${APPDIR}/x', steps: ['cmake -B build', 'ninja -C build'], artifacts: ['build/x', 'data', '*.md'], timeout: 90m, needs: {commands: [cmake, c++], pkg-config: [gtkmm-3.0], apt: [libgtkmm-3.0-dev]}}\ndesktop: {exec: x}\n",
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

func TestHomepage(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct{ body, want string }{
		"gh":    {"source: {type: github-release, repo: o/r}", "https://github.com/o/r"},
		"fj":    {"source: {type: forgejo, host: git.example.org, repo: o/r}", "https://git.example.org/o/r"},
		"gl":    {"source: {type: gitlab, project: g/p}\nasset: '*.zip'", "https://gitlab.com/g/p"},
		"fp":    {"source: {type: flatpak, app: org.x.Y, remote: flathub}\ninstall: {type: flatpak}", "https://flathub.org/apps/org.x.Y"},
		"etag":  {"source: {type: http-etag, url: 'https://x.example/a.AppImage'}", ""},
		"given": {"homepage: https://x.example\nsource: {type: github-release, repo: o/r}", "https://x.example"},
		"git":   {"source: {type: git, url: 'https://codeberg.org/o/r.git'}\ninstall: {dest: '${APPDIR}/r', steps: [make], artifacts: [r]}", "https://codeberg.org/o/r"},
	} {
		a, err := LoadFile(write(t, dir, name+".yaml", c.body+"\n"), vars)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := a.HomepageURL(); got != c.want {
			t.Errorf("%s: homepage = %q, want %q", name, got, c.want)
		}
	}
	if _, err := LoadFile(write(t, dir, "bad.yaml", "homepage: x.example\nsource: {type: github-release, repo: o/r}\n"), vars); err == nil {
		t.Error("a homepage without a scheme passed validation")
	}
}

func TestDesktop(t *testing.T) {
	dir := t.TempDir()
	src := "source: {type: github-release, repo: o/r}\n"
	a, err := LoadFile(write(t, dir, "a.yaml", src+"asset: '*.zip'\ninstall: {type: extract, dest: '${APPDIR}/a', executables: [bin/a]}\ndesktop: {args: '%f', icon: share/a.png, terminal: true}\n"), vars)
	if err != nil {
		t.Fatal(err)
	}
	if a.Program() != "/apps/a/bin/a" || a.Desktop.Args != "%f" || a.Desktop.Terminal == nil || !*a.Desktop.Terminal {
		t.Errorf("program %q, desktop %+v", a.Program(), a.Desktop)
	}
	b, err := LoadFile(write(t, dir, "b.yaml", src+"desktop: false\n"), vars)
	if err != nil || !b.Desktop.Off || b.Program() != "/apps/appimages/b" {
		t.Errorf("desktop: false: %v, %+v", err, b)
	}
	for name, body := range map[string]string{
		"escapes":  src + "asset: '*.zip'\ninstall: {type: extract, dest: /x}\ndesktop: {exec: ../../bin/sh}\n",
		"absolute": src + "asset: '*.zip'\ninstall: {type: extract, dest: /x}\ndesktop: {exec: /bin/sh}\n",
		"file":     src + "desktop: {exec: x}\n",
		"icon":     src + "desktop: {icon: 'http://x.example/i.png'}\n",
		"typo":     src + "desktop: {icons: x}\n",
		"true":     src + "desktop: true\n",
		"deb":      src + "asset: '*.deb'\ninstall: {type: deb}\ndesktop: {args: x}\n",
	} {
		if _, err := LoadFile(write(t, dir, name+".yaml", body), vars); err == nil {
			t.Errorf("%s passed validation", name)
		}
	}
}

// A build installs its artifacts by base name; the first one is the program
// unless desktop.exec says otherwise. The install type is implied by the source.
func TestBuildDefaults(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadFile(write(t, dir, "a.yaml", "source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/a', steps: [make], artifacts: ['build/bin/a-gtk', docs]}\n"), vars)
	if err != nil {
		t.Fatal(err)
	}
	if a.Install.Type != InstallBuild || a.Program() != "/apps/a/a-gtk" || a.Install.BuildTimeout() != DefaultBuildTimeout {
		t.Errorf("install %+v, program %q, timeout %s", a.Install, a.Program(), a.Install.BuildTimeout())
	}
	b, err := LoadFile(write(t, dir, "b.yaml", "source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPDIR}/b', steps: [make], artifacts: ['build/*'], timeout: 30m}\ndesktop: {exec: bin/b}\n"), vars)
	if err != nil {
		t.Fatal(err)
	}
	if b.Program() != "/apps/b/bin/b" || b.Install.BuildTimeout() != 30*time.Minute {
		t.Errorf("program %q, timeout %s", b.Program(), b.Install.BuildTimeout())
	}
	c, err := LoadFile(write(t, dir, "c.yaml", "source: {type: git, url: 'https://x/r'}\ninstall: {dest: '${APPIMAGEDIR}', steps: [make], artifacts: [{from: 'build/C-*.AppImage', to: C.AppImage}, LICENSE]}\n"), vars)
	if err != nil {
		t.Fatal(err)
	}
	if c.Program() != "/apps/appimages/C.AppImage" || c.Install.Artifacts[1].From != "LICENSE" {
		t.Errorf("program %q, artifacts %+v", c.Program(), c.Install.Artifacts)
	}
}
