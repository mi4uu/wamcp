NAME := whatsupd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX ?= $(HOME)/.local

.PHONY: build test install run demo clean dist

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(NAME) ./cmd/whatsupd

test:
	go test ./...

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/$(NAME) $(PREFIX)/bin/$(NAME)
	@if [ -f $(HOME)/Library/LaunchAgents/dev.whatsupd.daemon.plist ] || [ -f $${XDG_CONFIG_HOME:-$(HOME)/.config}/systemd/user/$(NAME).service ]; then $(PREFIX)/bin/$(NAME) service restart; fi

run: build
	./bin/$(NAME) serve

# Web client with synthetic data on http://127.0.0.1:8799 (sign-in link in the output).
demo:
	WHATSUPD_DEMO_ADDR=127.0.0.1:8799 go test ./internal/web -run TestDemoServer -timeout 0 -v

PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

DIST := $(CURDIR)/dist/$(VERSION)
NFPM ?= go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.46.3

# Release archives in dist/<version>/: one static binary per platform, .deb,
# .rpm and Arch packages for Linux, plus SHA256SUMS. Refuses to reuse a directory, so old archives never get mixed in.
dist:
	@printf '%s' '$(VERSION)' | grep -Eq '^v?[0-9A-Za-z][0-9A-Za-z.+-]*$$' || \
		{ echo "VERSION must look like v1.2.3, got '$(VERSION)'" >&2; exit 1; }
	@if [ -e '$(DIST)' ]; then echo "$(DIST) already exists; remove it or use another VERSION" >&2; exit 1; fi
	mkdir -p '$(DIST)'
	sed 's|@NAME@|$(NAME)|g' packaging/systemd.service > '$(DIST)/.$(NAME).service'
	sed 's|@NAME@|$(NAME)|g' packaging/postinstall.sh > '$(DIST)/.postinstall.sh'
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=$(NAME)_$(VERSION)_$${os}_$${arch}; \
		mkdir -p "$(DIST)/$$name"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$(DIST)/$$name/$(NAME)" ./cmd/whatsupd; \
		cp LICENSE README.md "$(DIST)/$$name/"; \
		tar -czf "$(DIST)/$$name.tar.gz" -C "$(DIST)" "$$name"; \
		if [ "$$os" = linux ]; then for pkg in deb rpm archlinux; do \
			sed -e 's|@NAME@|$(NAME)|g' -e "s|@BINARY@|$(DIST)/$$name/$(NAME)|" -e 's|@DIST@|$(DIST)|' \
				packaging/nfpm.yaml > "$(DIST)/.nfpm-$$arch.yaml"; \
			ARCH=$$arch VERSION='$(VERSION)' $(NFPM) pkg --config "$(DIST)/.nfpm-$$arch.yaml" --packager $$pkg --target '$(DIST)/'; \
		done; fi; \
	done
	cd '$(DIST)' && find . -maxdepth 1 -type f -name '$(NAME)*' | sed 's|^\./||' | sort | xargs shasum -a 256 > SHA256SUMS

clean:
	rm -rf bin
