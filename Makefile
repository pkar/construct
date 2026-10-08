GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)
BIN     := bin/construct

# Install dir: ~/.local/bin by default; PREFIX maps to $(PREFIX)/bin and an
# explicit BINDIR wins over both. DESTDIR is prepended for packaging.
ifdef PREFIX
BINDIR ?= $(PREFIX)/bin
else
BINDIR ?= $(HOME)/.local/bin
endif

.DEFAULT_GOAL := help
.PHONY: help all build install uninstall clean test vet fmt check

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
		'Variables' \
		'  BINDIR=DIR   install directory (default ~/.local/bin)' \
		'  PREFIX=DIR   install into DIR/bin' \
		'  DESTDIR=DIR  staging root for packaging' \
		'  VERSION=V    version stamped into the binary (default git describe)'

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
	rm -rf bin

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

check: fmt vet test
