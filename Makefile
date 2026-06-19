# netscope — build & dev tasks
# macOS / Go. Live capture needs root; offline replay (make run-pcap) does not.

BIN_DIR   := bin
DAEMON    := $(BIN_DIR)/netscoped
CLI       := $(BIN_DIR)/netscope
PREFIX    ?= /usr/local
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X github.com/doldoldol21/netscope/internal/buildinfo.Version=$(VERSION)

# Resolve the wails CLI even when $(go env GOPATH)/bin is not on PATH
# (the usual cause of "wails: command not found" after `go install`).
GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
GOBIN_DIR := $(shell go env GOPATH)/bin
endif
WAILS := $(shell command -v wails 2>/dev/null || echo $(GOBIN_DIR)/wails)

.DEFAULT_GOAL := build

.PHONY: build
build: $(DAEMON) $(CLI) ## Build daemon and CLI

$(DAEMON): $(shell find . -name '*.go') go.mod
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o $(DAEMON) ./cmd/netscoped

$(CLI): $(shell find . -name '*.go') go.mod
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(CLI) ./cmd/netscope

.PHONY: icons
icons: ## Regenerate app icons from assets/app-icon.svg
	./scripts/gen-icons.sh

.PHONY: app
app: ## Build the menu-bar app (netscope.app) — daemon bundled, dashboard inside
	./scripts/build-app.sh

# Dev socket: a user-writable path so the demo + the app work without sudo
# (the production default /var/run/netscope/... needs root to create).
DEV_SOCK := /tmp/netscope-dev.sock

.PHONY: demo
demo: build ## One command: synthetic daemon + menu-bar app (no root, Ctrl-C to stop)
	@test -x desktop/build/bin/netscope.app/Contents/MacOS/netscope || $(MAKE) app
	@rm -f $(DEV_SOCK)
	@echo "synthetic daemon on $(DEV_SOCK) — look at the menu bar. Ctrl-C to stop."
	@$(DAEMON) --demo --no-store --sock $(DEV_SOCK) & \
	 DPID=$$!; trap 'kill $$DPID 2>/dev/null; rm -f $(DEV_SOCK)' EXIT INT TERM; \
	 sleep 1; \
	 NETSCOPE_SOCK=$(DEV_SOCK) desktop/build/bin/netscope.app/Contents/MacOS/netscope

.PHONY: demo-daemon
demo-daemon: build ## Just the synthetic daemon (pair with `make app-dev` for UI hot-reload)
	$(DAEMON) --demo --no-store --sock $(DEV_SOCK)

.PHONY: app-dev
app-dev: ## Run the native app in Wails dev mode against the dev socket
	@test -x "$(WAILS)" || { echo "wails not found — installing…"; go install github.com/wailsapp/wails/v2/cmd/wails@latest; }
	cd desktop && NETSCOPE_SOCK=$(DEV_SOCK) "$(WAILS)" dev

.PHONY: run
run: build ## Run the daemon live (requires sudo for packet capture)
	sudo $(DAEMON)

.PHONY: run-pcap
run-pcap: build ## Replay a pcap (pipeline test; apps show as "unknown"): make run-pcap PCAP=file.pcap
	$(DAEMON) --pcap $(PCAP) --no-store --print --sock $(DEV_SOCK)

.PHONY: capture-sample
capture-sample: ## Capture 20s of traffic to testdata/sample.pcap (needs sudo)
	@mkdir -p testdata
	sudo tcpdump -i any -w testdata/sample.pcap -G 20 -W 1 'ip or ip6' 2>/dev/null || true

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: cover
cover: ## Run tests with coverage summary
	go test -cover ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## gofmt all sources
	gofmt -w $(shell find . -name '*.go' -not -path './vendor/*')

.PHONY: install
install: build ## Install CLI/daemon binaries into $(PREFIX)/bin and the launchd daemon
	sudo install -m 0755 $(DAEMON) $(PREFIX)/bin/netscoped
	sudo install -m 0755 $(CLI) $(PREFIX)/bin/netscope
	sudo ./scripts/install.sh

.PHONY: uninstall
uninstall: ## Remove binaries and the launchd daemon
	sudo ./scripts/install.sh --uninstall
	sudo rm -f $(PREFIX)/bin/netscoped $(PREFIX)/bin/netscope

.PHONY: package
package: ## Build + bundle + sign everything into dist/ (ad-hoc; see scripts/package.sh)
	./scripts/package.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) desktop/build/bin dist

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
