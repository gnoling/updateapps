# Writing definitions

One YAML file per app, `<id>.yaml`. The id is the file name: it's what you type as a
filter and what state is recorded under. A definition says where to find the latest
version (`source:`) and what to do with the download (`install:`).

```yaml
name: Gearboy                                  # default: the id
description: Nintendo Game Boy and Game Boy Color emulator
category: emulators                            # free text
source:
  type: github-release
  repo: drhelius/Gearboy
asset: '*ubuntu*24.04*x64.zip'
install:
  type: extract
  dest: ${APPDIR}/gearboy
  executables: [gearboy]
```

Other top-level keys: `enabled: false` (runs only when named), `notes:` (why a pattern is
odd, what upstream changed), `notice:` (printed as ACTION NEEDED after an install),
`post:` (shell hooks), `insecure: true` (skip TLS verification).

`${APPDIR}`, `${APPIMAGEDIR}`, `${HOME}` and `${name}` (the id) expand in `dest` and `notice`.

Check your work with `updateapps --defs DIR validate` and `updateapps --defs DIR check ID -v`.
Unknown keys are errors, so typos don't pass silently. [examples/apps.d](../examples/apps.d)
has a working file for most of what's below.

## Sources

The version a source reports is only ever compared for equality with the recorded one.

### github-release

```yaml
source:
  type: github-release
  repo: owner/name
  channel: latest          # latest (default; includes prereleases) | stable
  # tag: continuous        # or track one fixed tag instead of a channel
  version_from: release    # release (default) | tag | published_at | asset.name | asset.updated_at
  skip_unmatched: true     # use the newest release that has a matching asset
```

For a rolling tag that upstream keeps recreating (`continuous`, `nightly`), the tag never
changes: use `version_from: published_at` or `asset.updated_at`. While such a release is
briefly missing, the app is skipped rather than failed.

### asset

Used by `github-release`, `forgejo` and `gitlab` to pick one file from a release.

```yaml
asset: '*x86_64.AppImage'            # a glob, or:
asset:
  regex: '^Obsidian-[0-9.]+\.AppImage$'
  exclude_regex: '\.(exe|apk|sig)$'
  pick: first                        # first (default) | last, among matches
```

Globs are case-sensitive; use `regex: '(?i)...'` otherwise. The default is `*AppImage`
(`\.AppImage$` for forgejo). No match is an error that lists what the release has.

### github-actions

The newest run's artifact. Needs a GitHub token.

```yaml
source:
  type: github-actions
  repo: owner/name
  workflow: Build            # the workflow's name, or its file (build.yml)
  branch: main               # recommended: otherwise any branch's build counts
  artifact: app-linux-x64    # or artifact_regex: '^app-.*-x64$'
  status: success            # success (default) | any
  event: push                # optional
```

Pull-request runs are never used unless `event:` asks for them. Artifacts are zips, so
pair this with `extract` or `extract-one`; a workflow that uploads a single file un-zipped
delivers the file itself, so use `file`.

### forgejo, gitlab

```yaml
source: {type: forgejo, host: codeberg.org, repo: owner/name}     # also Gitea
source: {type: gitlab, project: group/name}                       # host: for self-hosted
```

The first release, with `asset:` choosing the file (gitlab matches release link names).

### html

Scrape a page with a CSS selector.

```yaml
source:
  type: html
  url: https://example.org/downloads/
  select: 'a[href$="x86_64.AppImage"]'
  attr: href                     # omit for the element's text
  include_regex: ''              # optional filters on the values found
  exclude_regex: '\.sig$'
  pick: first                    # first (default) | last, after de-duplication
  version: basename              # basename (default) | full | regex:<pattern with one group>
  download: ''                   # optional template; default is the value found
  headers: {User-Agent: curl/8}  # optional
```

Relative links resolve against the page. `download` is a Go template with `.Value`
(resolved), `.Raw`, `.Basename` and `.Version`. `command:` instead of `url:` parses a shell
command's output, for pages that need a real browser; it requires a trusted repository.

### http-etag

One fixed URL whose content changes in place. The version is its ETag (or Last-Modified).

```yaml
source: {type: http-etag, url: https://example.org/app-latest.AppImage}
```

### json, yaml

Fetch a document and query it with jq.

```yaml
source:
  type: yaml
  url: https://example.org/latest.yaml
  version: '.latest[0].version'
  download: 'https://example.org/{{.Version}}/app-{{.Version}}-linux.tar.xz'   # or url_jq: '<jq>'
```

### flatpak

An app flatpak updates from a remote; updateapps asks it to. The version is the commit the
remote offers. Implies `install: {type: flatpak}`.

```yaml
source: {type: flatpak, ref: https://dl.flathub.org/repo/appstream/org.example.App.flatpakref}
source: {type: flatpak, remote: flathub, app: org.example.App, branch: stable}
```

### script

Lua, when nothing above fits: see [LUA.md](LUA.md).

## Installs

```yaml
install:                       # the default when install: is omitted
  type: file                   # the download is the program
  dest: ${APPIMAGEDIR}/${name} # a trailing / means "this directory, named by id"
```

```yaml
install:
  type: extract                # unpack an archive into a directory
  dest: ${APPDIR}/gearboy
  strip: 1                     # like tar --strip-components
  exclude: ['*.json']          # never written: protects user settings
  executables: [gearboy, 'bin/*']
```

Extraction overlays `dest` and never deletes. Formats (tar with gzip/bzip2/xz/zstd, zip, 7z)
are detected from content. Members that would land outside `dest` are rejected.

```yaml
install:
  type: extract-one            # one file out of an archive
  member: 'Cemu-*.AppImage'    # a glob on the path or the file name; default: the first file
  member_ci: false
  dest: ${APPIMAGEDIR}/Cemu.AppImage
```

```yaml
install: {type: none}          # only post hooks use the download ($FILE)
install: {type: deb, package: bat}            # apt; package: is optional
install: {type: flatpak, scope: user}         # a .flatpak bundle or .flatpakref
```

`nested: true`, on any type, unwraps an archive that merely wraps the real download
(a tarball, `.deb` or `.flatpak` inside a zip).

`deb` and system-scope `flatpak` installs, like hooks, need a trusted repository
([REPOSITORIES.md](REPOSITORIES.md#trust)).

## Post hooks

```yaml
post:
  - scp "$FILE" server:/srv/app/app.jar
  - ssh server "systemctl restart app"
```

Run with `sh -c`, in order, after a successful install, with `FILE`, `DEST`, `VERSION`,
`NAME`, `APPDIR` and `APPIMAGEDIR` set. A failing hook fails the app: nothing is recorded,
so the next run installs and runs the hooks again. Each has ten minutes.
