VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX ?= $(HOME)/.local

.PHONY: build test install run demo clean dist

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/wamcp ./cmd/wamcp

test:
	go test ./...

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/wamcp $(PREFIX)/bin/wamcp
	@if [ -f $(HOME)/Library/LaunchAgents/dev.wamcp.daemon.plist ] || [ -f $${XDG_CONFIG_HOME:-$(HOME)/.config}/systemd/user/wamcp.service ]; then $(PREFIX)/bin/wamcp service restart; fi

run: build
	./bin/wamcp serve

# Web client with synthetic data on http://127.0.0.1:8799 (sign-in link in the output).
demo:
	WAMCP_DEMO_ADDR=127.0.0.1:8799 go test ./internal/web -run TestDemoServer -timeout 0 -v

PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

DIST := $(CURDIR)/dist/$(VERSION)

# Release archives in dist/<version>/: one static binary per platform, plus
# SHA256SUMS. Refuses to reuse a directory, so old archives never get mixed in.
dist:
	@printf '%s' '$(VERSION)' | grep -Eq '^v?[0-9A-Za-z][0-9A-Za-z.+-]*$$' || \
		{ echo "VERSION must look like v1.2.3, got '$(VERSION)'" >&2; exit 1; }
	@if [ -e '$(DIST)' ]; then echo "$(DIST) already exists; remove it or use another VERSION" >&2; exit 1; fi
	mkdir -p '$(DIST)'
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=wamcp_$(VERSION)_$${os}_$${arch}; \
		mkdir -p "$(DIST)/$$name"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o "$(DIST)/$$name/wamcp" ./cmd/wamcp; \
		cp LICENSE README.md "$(DIST)/$$name/"; \
		tar -czf "$(DIST)/$$name.tar.gz" -C "$(DIST)" "$$name"; \
	done
	cd '$(DIST)' && shasum -a 256 *.tar.gz > SHA256SUMS

clean:
	rm -rf bin
