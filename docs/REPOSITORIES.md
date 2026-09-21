# Definition repositories

Definitions aren't part of updateapps. They come from repositories listed in
`~/.config/updateapps/config.yaml`, each stored in `apps.d/<name>/` beside it:

```
~/.config/updateapps/
  config.yaml
  apps.d/
    main/        fetched; replaced on every pull, so don't edit
    someone/     fetched
    local/       yours; read last, so it wins
```

With no `repositories:` key you get the public set, opt-in:

```yaml
repositories:
  - name: main
    url: https://github.com/gnoling/updateapps-definitions
    default: disabled
```

A fuller example (`repositories: []` means none at all):

```yaml
repositories:                 # read in order; a later one wins on the same id
  - name: main
    url: https://github.com/gnoling/updateapps-definitions
  - name: someone
    url: https://codeberg.org/someone/their-definitions
    branch: main
    default: disabled         # opt-in: only what enabled: names will run
  - name: private
    url: https://github.com/me/my-private-definitions   # a GitHub token reaches private repos
    trusted: true
  - name: local               # exists regardless; listed here only to trust it
    trusted: true

enabled:  [someone/their-app]   # this machine's say, by id or repo/id
disabled: [mesen]
auto_pull: false                # true: pull before every update
```

## Fetched or yours

- **With `url:`**, the repository's definitions and `repo.yaml` are fetched into
  `apps.d/<name>/`: automatically the first time they're needed, after that by
  `updateapps repos pull` (or `auto_pull: true`). The URL is a GitHub, Forgejo/Gitea or GitLab
  repository, or a direct `.tar.gz`/`.zip` link. updateapps downloads the archive the host
  builds on request, so there's no git requirement and nothing to package. An unchanged
  repository costs one conditional request. A pull is swapped in only once it loads; one
  that would leave no definitions is refused. A pull only replaces folders it fetched
  (marked by `.updateapps-fetched.json`).
- **Without `url:`** the folder is yours and never written to. `path:` puts it elsewhere,
  such as a git checkout.
- **`local`** always exists. To change a fetched definition, copy it there.

A `.yaml` outside a repository folder, or a folder no repository claims, is reported
rather than silently ignored.

`updateapps repos` lists repositories, `list` shows each app's repository, and `validate`
reports overrides and anything needing trust. `--defs DIR` reads one flat folder instead,
trusted: use it to check a repository you're writing.

## Which apps run

The definition's `enabled:`, overridden by the repository's `default: disabled`, overridden
by your `enabled:`/`disabled:` lists. Naming a disabled app on the command line runs it once.

`updateapps enable ID...` and `disable ID...` edit those lists. `enable --all [REPO...]` and
`disable --all` set the repositories' `default:` instead, which also covers definitions
added later; apps you've listed by name keep their setting.

## Trust

Whoever controls a repository controls what its definitions do on your machine. Unless
you set `trusted: true`, its definitions may not:

- run shell commands (`post:`, `source.command`),
- install as root (`install: deb`, system-wide flatpaks),
- install outside `appdir`/`appimagedir`.

Those show as **needs trust** and are skipped; the rest of the repository works. Lua is
always allowed: it's sandboxed. `repos pull` says when a definition newly needs trust.
Trust is never implied, not even for `local`.

## Publishing one

YAML files in `apps.d/` (or at the top level), one app per file, named `<id>.yaml`. Lua
files for `script_file:` sit beside them. Optional `repo.yaml`:

```yaml
name: My apps
description: What's in here.
schema: 1        # older builds of updateapps then say so, instead of failing on new fields
```
