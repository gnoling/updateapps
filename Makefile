# The CLI must stay a static, GUI-free binary: CGO_ENABLED=0 is not optional.
PREFIX ?= $(HOME)/.local/bin

.PHONY: build build-gui test install install-gui
build:
	CGO_ENABLED=0 go build -o bin/updateapps ./cmd/updateapps

# The GUI needs cgo and the OpenGL/X11 development headers (see README).
build-gui:
	go build -o bin/updateapps-gui ./cmd/updateapps-gui

test:
	go vet ./...
	go test -race ./...

# Copies rather than symlinks, so a broken work-in-progress build can't break
# the installed command.
install: build
	install -D -m 0755 bin/updateapps $(PREFIX)/updateapps

# Also a launcher and icon; DATADIR follows the XDG default for a per-user install.
DATADIR ?= $(HOME)/.local/share

install-gui: build-gui
	install -D -m 0755 bin/updateapps-gui $(PREFIX)/updateapps-gui
	install -D -m 0644 internal/gui/icon.svg $(DATADIR)/icons/hicolor/scalable/apps/updateapps.svg
	sed 's|@BIN@|$(PREFIX)|g' packaging/updateapps-gui.desktop > $(DATADIR)/applications/updateapps-gui.desktop
	@update-desktop-database $(DATADIR)/applications 2>/dev/null || true
	@gtk-update-icon-cache -q $(DATADIR)/icons/hicolor 2>/dev/null || true
