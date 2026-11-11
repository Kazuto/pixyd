PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
UNITDIR ?= $(HOME)/.config/systemd/user
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test hwtest lint run install uninstall clean

## build: the static binary in bin/pixyd
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/pixyd ./cmd/pixyd

## test: unit tests; no camera needed
test:
	go test -race ./...

## hwtest: read-only checks against a connected camera
hwtest:
	PIXYD_HW=read go test -count=1 -v -run Hardware ./internal/hid
	go test -count=1 -v -run Live ./internal/v4l2 ./internal/sysfs

## lint: golangci-lint
lint:
	$(GOLANGCI_LINT) run ./...

## run: build and run in the foreground
run: build
	./bin/pixyd -debug

## install: binary to $(BINDIR), user unit to $(UNITDIR), enabled and (re)started
install: build
	install -Dm755 bin/pixyd $(BINDIR)/pixyd
	install -d $(UNITDIR)
	sed 's|@BINDIR@|$(BINDIR)|' deploy/pixyd.service > $(UNITDIR)/pixyd.service
	systemctl --user daemon-reload
	systemctl --user enable pixyd.service
	systemctl --user restart pixyd.service

## uninstall: stop, disable and remove what install put in place
uninstall:
	-systemctl --user disable --now pixyd.service
	rm -f $(UNITDIR)/pixyd.service $(BINDIR)/pixyd
	systemctl --user daemon-reload

clean:
	rm -rf bin
