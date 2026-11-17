# Axis IP Tool
#
#   make                 alles bauen (GUI benötigt gtk4- und libadwaita-Entwicklungspakete)
#   sudo make install    installieren (GUI, CLI)
#   sudo make uninstall  vollständig entfernen
#   make packages        .rpm/.deb/.pkg.tar.zst nach dist/ (benötigt nfpm)
#   make flatpak         Flatpak-Bundle nach dist/ (benötigt flatpak-builder)

VERSION ?= $(shell sh packaging/scripts/version.sh)
ARCH    ?= $(shell go env GOARCH)
PREFIX  ?= /usr
DESTDIR ?=
BUILDFLAGS := -trimpath
LDFLAGS := -s -w

APP_ID  := io.github.reschcloud.AxisIpTool
BIN     := build/axis-ip-tool build/axisctl

.PHONY: all build gui cli test install install-app uninstall \
        packages flatpak clean

all: build
build: $(BIN)

build/axis-ip-tool: $(shell find cmd/axis-ip-tool internal -name '*.go') go.mod
	go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/axis-ip-tool

build/axisctl: $(shell find cmd/axisctl internal -name '*.go') go.mod
	CGO_ENABLED=0 go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/axisctl

gui: build/axis-ip-tool
cli: build/axisctl

test:
	go vet ./internal/... ./cmd/axisctl
	go test ./internal/...

install: install-app

install-app: build/axis-ip-tool build/axisctl
	install -Dm755 build/axis-ip-tool $(DESTDIR)$(PREFIX)/bin/axis-ip-tool
	install -Dm755 build/axisctl      $(DESTDIR)$(PREFIX)/bin/axisctl
	install -Dm644 data/$(APP_ID).desktop       $(DESTDIR)$(PREFIX)/share/applications/$(APP_ID).desktop
	install -Dm644 data/$(APP_ID).metainfo.xml  $(DESTDIR)$(PREFIX)/share/metainfo/$(APP_ID).metainfo.xml
	install -Dm644 data/icons/hicolor/scalable/apps/$(APP_ID).svg \
		$(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps/$(APP_ID).svg
	install -Dm644 data/icons/hicolor/symbolic/apps/$(APP_ID)-symbolic.svg \
		$(DESTDIR)$(PREFIX)/share/icons/hicolor/symbolic/apps/$(APP_ID)-symbolic.svg
	@[ -n "$(DESTDIR)" ] || sh packaging/scripts/app-postinstall.sh

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/axis-ip-tool $(DESTDIR)$(PREFIX)/bin/axisctl \
	      $(DESTDIR)$(PREFIX)/share/applications/$(APP_ID).desktop \
	      $(DESTDIR)$(PREFIX)/share/metainfo/$(APP_ID).metainfo.xml \
	      $(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps/$(APP_ID).svg \
	      $(DESTDIR)$(PREFIX)/share/icons/hicolor/symbolic/apps/$(APP_ID)-symbolic.svg
	-sh packaging/scripts/app-postinstall.sh

packages: build
	@mkdir -p dist
	@command -v nfpm >/dev/null || { echo "nfpm fehlt: GOTOOLCHAIN=auto go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"; exit 1; }
	for fmt in rpm deb archlinux; do \
	  VERSION=$(VERSION) ARCH=$(ARCH) nfpm package -f packaging/nfpm-app.yaml -p $$fmt -t dist/ || exit 1; \
	done
	@ls -1 dist/

flatpak:
	@mkdir -p dist
	flatpak-builder --force-clean --user --install-deps-from=flathub \
		--repo=build/flatpak-repo build/flatpak packaging/$(APP_ID).yml
	flatpak build-bundle build/flatpak-repo dist/$(APP_ID).$(shell uname -m).flatpak $(APP_ID)

clean:
	rm -rf build dist
