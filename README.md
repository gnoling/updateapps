# updateapps

Keeps apps that don't come from a package manager up to date: AppImages, portable
tarballs and zips, CI nightlies, `.deb`s and flatpaks. For each app it finds the latest
version upstream, and if that's new, downloads and installs it.

What to track is described in small YAML **definitions**, which live in git repositories
separate from the program. A [public set](https://github.com/gnoling/updateapps-definitions)
of about 130 (mostly emulators, game ports and recompilations) is there to pick from, and
you can add your own or other people's.

```
$ updateapps
Gearboy (3.8.15) — Nintendo Game Boy and Game Boy Color emulator
Obsidian (v1.13.7) — Markdown-based knowledge base / note-taking app
Updated 2 out of 41 apps in 0 minute(s), 9 second(s)
```

Linux, a single static binary, no runtime dependencies.

## Getting started

**1. Install.**

```
go install github.com/gnoling/updateapps/cmd/updateapps@latest
```

or from a checkout, `make install` (to `~/.local/bin`; `PREFIX=` to change).

**2. See what's available.** No setup is needed first. The first run fetches the public
definitions into `~/.config/updateapps/apps.d/main/` and installs nothing:

```
updateapps list
```

**3. Choose apps.** Everything starts disabled. Name what you want by id:

```
updateapps enable dolphin rpcs3
```

If you just want to enable everything:

```
updateapps enable --all
```

Either creates `~/.config/updateapps/config.yaml` if there isn't one, and later edits leave
the rest of the file as you wrote it. Every key it takes is in the
[sample config](examples/config.yaml).

**4. Run it.** `check` says what would happen; a bare `updateapps` does it:

```
updateapps check                     # what's new, downloading nothing
updateapps                           # update everything enabled
updateapps dolphin                   # only apps matching "dolphin"
```

Apps land under `~/apps`, AppImages under `~/apps/appimages`; change that with `appdir:`
and `appimagedir:` in the config. Each definition says where its app goes, so if you
already have some of these installed, point `appdir:` at them and run
`updateapps mark-current` once to record them as current instead of downloading again.

**5. Add a GitHub token** (optional, but do it): `github_token:` in the config,
`$GITHUB_TOKEN`, or a logged-in `gh`. Without one GitHub allows 60 requests an hour,
and CI artifacts can't be downloaded at all.

**6. Your own definitions.** Drop a YAML file in `~/.config/updateapps/apps.d/local/`
(see [writing definitions](docs/DEFINITIONS.md)), or add other people's repositories to
the config (see [definition repositories](docs/REPOSITORIES.md)). Definitions from a
repository you haven't marked `trusted: true` can't run shell commands, install as root,
or write outside your apps directories.

## Commands

| | |
|---|---|
| `updateapps [FILTER...]` | update enabled apps, or those matching a filter |
| `check [FILTER...]` | report new versions without downloading |
| `list [FILTER...]` | apps, installed versions, status |
| `enable ID...`, `disable ID...` | turn apps on or off (edits the config); `--all [REPO]` for everything |
| `show ID` | one app's definition and state |
| `mark-current [FILTER...]` | record already-installed apps as current, without downloading |
| `validate` | check every definition |
| `repos`, `repos pull` | list or refresh definition repositories |

A filter is a case-insensitive substring of an app's id, name or repo/URL. Flags:
`-f` reinstall, `-v`/`-vv` detail, `--dry-run`, `-j N` parallel jobs, `--config FILE`,
`--defs DIR`. Exit status: 0 fine, 1 an app failed, 2 a config or definition problem.

## How it behaves

- Apps are processed in parallel. Each app's output appears as one block when it finishes.
- Downloads are staged and moved into place, so a failure or Ctrl-C never leaves a
  half-installed app, and replacing a running AppImage is safe.
- Extracting over an existing install never deletes files, so saves and settings survive.
- A version is recorded only after the install, and any hooks, succeed. Failures retry next run.
- `.deb`s install in one `apt` transaction at the end of a run. Without root the download
  is kept and the command to run is printed.
- Definitions from a repository you haven't marked `trusted` can't run shell commands,
  install as root, or write outside your apps directories.

## Documentation

- [Writing definitions](docs/DEFINITIONS.md), with [examples](examples/apps.d)
- [Definition repositories](docs/REPOSITORIES.md): using, publishing, trust
- [Lua scripts](docs/LUA.md), for upstreams nothing declarative can describe
- [Sample config](examples/config.yaml)

## License

[MIT](LICENSE). The [public definitions](https://github.com/gnoling/updateapps-definitions)
are CC0.
