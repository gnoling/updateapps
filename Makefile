# The CLI must stay a static, GUI-free binary: CGO_ENABLED=0 is not optional.
PREFIX ?= $(HOME)/.local/bin

.PHONY: build test install
build:
	CGO_ENABLED=0 go build -o bin/updateapps ./cmd/updateapps

test:
	go vet ./...
	go test -race ./...

# Copies rather than symlinks, so a broken work-in-progress build can't break
# the installed command.
install: build
	install -D -m 0755 bin/updateapps $(PREFIX)/updateapps
