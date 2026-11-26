# IP Tool for AXIS (Linux)
#
#   make                 build everything (the GUI needs gtk4 and libadwaita development packages)
#   sudo make install    install (GUI, CLI)
#   sudo make uninstall  remove completely
#   make packages        .rpm/.deb/.pkg.tar.zst into dist/ (needs nfpm)
#   make flatpak         Flatpak bundle into dist/ (needs flatpak-builder)

VERSION ?= $(shell sh packaging/scripts/version.sh)
ARCH    ?= $(shell go env GOARCH)
PREFIX  ?= /usr
DESTDIR ?=
BUILDFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

APP_ID  := io.github.reschcloud.IPToolForAXIS
BIN     := build/ip-tool-for-axis build/ip-tool-for-axis-cli

.PHONY: all build gui cli test install install-app uninstall \
        packages flatpak clean

all: build
build: $(BIN)

build/ip-tool-for-axis: $(shell find cmd/ip-tool-for-axis internal -name '*.go') go.mod
	go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/ip-tool-for-axis

build/ip-tool-for-axis-cli: $(shell find cmd/ip-tool-for-axis-cli internal -name '*.go') go.mod
	CGO_ENABLED=0 go build $(BUILDFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/ip-tool-for-axis-cli

gui: build/ip-tool-for-axis
cli: build/ip-tool-for-axis-cli

test:
	go vet ./internal/... ./cmd/ip-tool-for-axis-cli
	go test ./internal/...

install: install-app

install-app: $(BIN)
	install -Dm755 build/ip-tool-for-axis     $(DESTDIR)$(PREFIX)/bin/ip-tool-for-axis
	install -Dm755 build/ip-tool-for-axis-cli $(DESTDIR)$(PREFIX)/bin/ip-tool-for-axis-cli
	install -Dm644 data/$(APP_ID).desktop       $(DESTDIR)$(PREFIX)/share/applications/$(APP_ID).desktop
	install -Dm644 data/$(APP_ID).metainfo.xml  $(DESTDIR)$(PREFIX)/share/metainfo/$(APP_ID).metainfo.xml
	install -Dm644 data/icons/hicolor/scalable/apps/$(APP_ID).svg \
		$(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps/$(APP_ID).svg
	install -Dm644 data/icons/hicolor/symbolic/apps/$(APP_ID)-symbolic.svg \
		$(DESTDIR)$(PREFIX)/share/icons/hicolor/symbolic/apps/$(APP_ID)-symbolic.svg
	@[ -n "$(DESTDIR)" ] || sh packaging/scripts/app-postinstall.sh

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/ip-tool-for-axis $(DESTDIR)$(PREFIX)/bin/ip-tool-for-axis-cli \
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
