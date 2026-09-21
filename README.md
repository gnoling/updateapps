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

## Install

```
go install github.com/gnoling/updateapps/cmd/updateapps@latest
```

or from a checkout, `make install` (to `~/.local/bin`; `PREFIX=` to change).

## Quick start

With no config file, updateapps uses the public definitions and installs nothing until
you choose what you want:

```
updateapps list                      # fetches the definitions, shows what's available
updateapps enable dolphin rpcs3      # or: updateapps enable --all
updateapps check                     # what's new, downloading nothing
updateapps                           # update everything enabled
updateapps dolphin                   # only apps matching "dolphin"
```

Apps install under `~/apps` (`appdir:` in `~/.config/updateapps/config.yaml`; see the
[sample config](examples/config.yaml)). `enable` and `disable` edit that file for you,
leaving the rest of it as you wrote it.

A GitHub token avoids the 60 requests/hour anonymous limit and is required for CI
artifacts: `github_token:` in the config, `$GITHUB_TOKEN`, or a logged-in `gh`.

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
