# Desktop integration

Off by default. With `desktop_integration: true` in the config (or "Add installed apps to
the applications menu" in the GUI's settings), every install also writes a launcher, so
the app shows up in your applications menu:

```
~/.local/share/applications/updateapps-<id>.desktop
~/.local/share/updateapps/icons/<id>.png        # or .svg
```

`updateapps desktop [FILTER...]` writes them for what's already installed, without
updating anything, and works whether or not the setting is on. `--dry-run` shows what it
would do. `updateapps desktop --remove [FILTER...]` deletes them again; with no filter,
every launcher updateapps wrote.

## Your own launchers win

- An app gets no launcher if any of your `.desktop` files starts its program, or starts
  anything else in the app's folder (a wrapper script, a second binary).
- Only files marked `X-Updateapps-Managed=true` are ever replaced or removed. To keep an
  edited copy, delete that line from it: it's yours from then on. Add `NoDisplay=true` as
  well and the app stays out of the menu.
- A launcher is rewritten on every install of its app, so edits to a managed one don't last.

## What goes in it

| Key | From |
|---|---|
| `Name`, `Comment` | the definition's `name:` and `description:` |
| `Exec` | the installed file; for `extract`, `desktop.exec` or the first of `install.executables`; for `build`, `desktop.exec` or the first artifact |
| `Path` | the program's folder (`extract` only) |
| `Icon` | `desktop.icon`, else the AppImage's own, else a top-level `<id>`, `<program>`, `icon` or `logo` `.svg`/`.png` in the app's folder |
| `Categories` | `desktop.categories`, else the AppImage's, else from `category:` (emulators: `Game;Emulator;`, ports and games: `Game;`, anything else: `Utility;`) |
| `Terminal`, `StartupWMClass` | `desktop.terminal` / `desktop.wm_class`, else the AppImage's |

AppImages carry a launcher and icon inside them, and that's what is read. It's read
directly, nothing is run. AppImages packed with DwarFS instead of squashfs can't be read
this way: they get a launcher without an icon unless the definition names one.

`deb` and `flatpak` installs bring their own launchers, and `install: none` has nothing
to start.

## In a definition

All optional:

```yaml
desktop:
  exec: bin/aegisub                  # extract and build only: the program, relative to install.dest
  args: '%f'                         # flags or a field code, after the program
  icon: share/icons/aegisub.svg      # relative to install.dest, or an https URL
  categories: AudioVideo;Video;
  terminal: false
  wm_class: aegisub
```

```yaml
desktop: false                       # a command-line tool or daemon: no launcher
```

`exec` and a local `icon` can't point outside `install.dest`.
