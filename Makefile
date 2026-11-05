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
	@if [ -f $(HOME)/Library/LaunchAgents/dev.wamcp.daemon.plist ]; then $(PREFIX)/bin/wamcp service restart; fi

run: build
	./bin/wamcp serve

# Web client with synthetic data on http://127.0.0.1:8799 (sign-in link in the output).
demo:
	WAMCP_DEMO_ADDR=127.0.0.1:8799 go test ./internal/web -run TestDemoServer -timeout 0 -v

PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

# Release archives in dist/: one static binary per platform, plus SHA256SUMS.
dist:
	mkdir -p dist
	set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=wamcp_$(VERSION)_$${os}_$${arch}; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$name/wamcp ./cmd/wamcp; \
		cp LICENSE README.md dist/$$name/; \
		tar -C dist -czf dist/$$name.tar.gz $$name; \
	done
	cd dist && shasum -a 256 wamcp_$(VERSION)_*.tar.gz > SHA256SUMS

clean:
	rm -rf bin
