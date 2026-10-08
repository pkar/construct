GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)
BIN     := bin/construct

# Release binaries; keep in sync with .github/workflows/release.yml.
DIST_TARGETS ?= linux/amd64 linux/arm64 darwin/arm64

# Install dir: ~/.local/bin by default; PREFIX maps to $(PREFIX)/bin and an
# explicit BINDIR wins over both. DESTDIR is prepended for packaging.
ifdef PREFIX
BINDIR ?= $(PREFIX)/bin
else
BINDIR ?= $(HOME)/.local/bin
endif

.DEFAULT_GOAL := help
.PHONY: help all build install uninstall clean test vet fmt check dist

help:
	@printf '%s\n' \
		'Build and install' \
		'  build      build $(BIN) for this machine' \
		'  install    install construct into $$BINDIR (default ~/.local/bin)' \
		'  uninstall  remove the installed binary' \
		'  clean      remove build output' \
		'' \
		'Checks' \
		'  fmt        check gofmt formatting' \
		'  vet        run go vet' \
		'  test       run tests with the race detector' \
		'  check      run fmt, vet, and test' \
		'' \
		'Release' \
		'  dist       cross-build dist/construct-OS-ARCH and dist/checksums.txt' \
		'             (CI runs this on v* tags and publishes a GitHub release)' \
		'' \
		'Variables' \
		'  BINDIR=DIR   install directory (default ~/.local/bin)' \
		'  PREFIX=DIR   install into DIR/bin' \
		'  DESTDIR=DIR  staging root for packaging' \
		'  VERSION=V    version stamped into the binary (default git describe)' \
		'  DIST_TARGETS="os/arch ..."  dist targets (default $(DIST_TARGETS))'

all: build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/construct

install: build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 $(BIN) "$(DESTDIR)$(BINDIR)/construct"
	@echo "installed $(BINDIR)/construct"
	@case ":$$PATH:" in *:"$(BINDIR)":*) ;; \
	*) echo "note: $(BINDIR) is not on your PATH" ;; esac

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/construct"

clean:
	rm -rf bin dist

dist:
	rm -rf dist
	mkdir -p dist
	@set -e; for target in $(DIST_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "build dist/construct-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath \
			-ldflags '$(LDFLAGS)' -o "dist/construct-$$os-$$arch" ./cmd/construct; \
	done
	cd dist && shasum -a 256 construct-* > checksums.txt

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

check: fmt vet test
