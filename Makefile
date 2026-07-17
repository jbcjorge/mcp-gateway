.PHONY: all build install uninstall clean test test-report fmt vet shadow lint vuln gosec gitleaks cyclomatic cognitive check tools release warm version tag

BINARY     := mcp-gateway
PREFIX     ?= $(HOME)/.local/bin
CONFIG_DIR := $(HOME)/.config/mcp-gateway
PLIST_DIR  := $(HOME)/Library/LaunchAgents
LOG_DIR    := $(HOME)/.local/var/log
REPORTS_DIR := reports

VERSION ?= $(shell \
	if git describe --tags --exact-match >/dev/null 2>&1; then \
		git describe --tags --exact-match; \
	else \
		echo "$$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)-dev-$$(date +%m%d%H%M)"; \
	fi)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X main.Version=$(VERSION) \
  -X main.Commit=$(COMMIT) \
  -X main.BuildDate=$(DATE)

# Default target
all: check build

## tools: Install development tools
tools:
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
	go install github.com/uudashr/gocognit/cmd/gocognit@latest
	go install golang.org/x/tools/go/analysis/passes/shadow/cmd/shadow@latest
	go install github.com/securego/gosec/v2/cmd/gosec@latest
	go install gotest.tools/gotestsum@latest
	go install github.com/boumenot/gocover-cobertura@latest

## build: Compile binary
build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .

## fmt: Format code
fmt:
	gofmt -s -w .

## vet: Run go vet
vet:
	go vet ./...

## shadow: Check for variable shadowing
shadow:
	go vet -vettool=$$(go env GOPATH)/bin/shadow ./...

## lint: Run staticcheck
lint:
	staticcheck ./...

## vuln: Run govulncheck
vuln:
	govulncheck ./...

## gosec: Security-focused static analysis
gosec:
	gosec -quiet ./...

## gitleaks: Scan for secrets
gitleaks:
	gitleaks detect --no-git -v

## cyclomatic: Check cyclomatic complexity (threshold: 15)
cyclomatic:
	@output=$$(gocyclo -over 15 .); \
	if [ -n "$$output" ]; then \
		echo "Cyclomatic complexity over 15:"; \
		echo "$$output"; \
		exit 1; \
	fi
	@gocyclo -avg . | grep '^Average'

## cognitive: Check cognitive complexity (threshold: 15)
cognitive:
	@output=$$(gocognit -over 15 .); \
	if [ -n "$$output" ]; then \
		echo "Cognitive complexity over 15:"; \
		echo "$$output"; \
		exit 1; \
	fi

## test: Run tests
test:
	go test -count=1 ./...

## test-report: Run tests with coverage and JUnit reports (for CI)
test-report:
	@mkdir -p $(REPORTS_DIR)
	gotestsum --junitfile $(REPORTS_DIR)/junit.xml -- -count=1 -coverprofile=$(REPORTS_DIR)/coverage.out -covermode=count ./...
	@go tool cover -html=$(REPORTS_DIR)/coverage.out -o $(REPORTS_DIR)/coverage.html
	gocover-cobertura < $(REPORTS_DIR)/coverage.out > $(REPORTS_DIR)/coverage.xml

## check: Run all quality gates (the "is this ready to push?" command)
check: fmt vet shadow lint vuln gosec gitleaks cyclomatic cognitive test

## install: Build, codesign (macOS), and install with launchd service
install: build
	@mkdir -p $(PREFIX) $(CONFIG_DIR) $(LOG_DIR)
	@test -f $(CONFIG_DIR)/config.json || cp resources/config.json $(CONFIG_DIR)/config.json
	@test -f $(CONFIG_DIR)/backends.json || cp resources/backends.json $(CONFIG_DIR)/backends.json
	@cp $(BINARY) $(PREFIX)/$(BINARY)
	@codesign -fs "mcp-gateway" -i io.github.jbcjorge.mcp-gateway $(BINARY) 2>/dev/null || codesign -s - -f -i io.github.jbcjorge.mcp-gateway $(BINARY) 2>/dev/null || true
	@if launchctl list 2>/dev/null | grep -q io.github.jbcjorge.mcp-gateway; then \
		PID=$$(launchctl list | grep io.github.jbcjorge.mcp-gateway | awk '{print $$1}'); \
		if [ "$$PID" != "-" ] && [ -n "$$PID" ]; then \
			kill $$PID 2>/dev/null && echo "restarted service (pid $$PID killed, launchd will respawn)"; \
		fi; \
	fi
	@echo "installed $(PREFIX)/$(BINARY) ($(VERSION))"
	@echo "config:   $(CONFIG_DIR)/config.json"
	@gsed -e 's|__BINARY__|$(PREFIX)/$(BINARY)|g' \
	     -e 's|__CONFIG__|$(CONFIG_DIR)/config.json|g' \
	     -e 's|__PORT__|19900|g' \
	     -e 's|__LOGDIR__|$(LOG_DIR)|g' \
	     -e 's|__HOME__|$(HOME)|g' \
	     -e 's|__PATH__|$(PATH)|g' \
	     resources/io.github.jbcjorge.mcp-gateway.plist.tpl > /tmp/io.github.jbcjorge.mcp-gateway.plist.new; \
	if [ -f $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist ]; then \
		if diff -q /tmp/io.github.jbcjorge.mcp-gateway.plist.new $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist >/dev/null 2>&1; then \
			rm -f /tmp/io.github.jbcjorge.mcp-gateway.plist.new; \
		else \
			echo ""; \
			echo "Service plist has changed."; \
			if launchctl list 2>/dev/null | grep -q io.github.jbcjorge.mcp-gateway; then \
				echo "Service is currently RUNNING. Updating will unload/reload it."; \
			fi; \
			printf "Update service? [y/N] "; \
			read ans; \
			case "$$ans" in \
				[yY]*) \
					launchctl unload $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist 2>/dev/null || true; \
					mv /tmp/io.github.jbcjorge.mcp-gateway.plist.new $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist; \
					echo "plist updated. Load with:"; \
					echo "  launchctl load $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist"; \
				;; \
				*) \
					echo "Service not updated."; \
					rm -f /tmp/io.github.jbcjorge.mcp-gateway.plist.new; \
				;; \
			esac; \
		fi; \
	else \
		mv /tmp/io.github.jbcjorge.mcp-gateway.plist.new $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist; \
		echo "service:  $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist"; \
		echo ""; \
		echo "Load with:"; \
		echo "  launchctl load $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist"; \
		echo ""; \
		echo "See INSTALL.md for full setup."; \
	fi

## uninstall: Remove binary and launchd service
uninstall:
	launchctl unload $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist 2>/dev/null || true
	rm -f $(PREFIX)/$(BINARY)
	rm -f $(PLIST_DIR)/io.github.jbcjorge.mcp-gateway.plist
	@echo "uninstalled"

## clean: Remove build artifacts
clean:
	rm -f $(BINARY)
	rm -rf $(REPORTS_DIR) dist/

## release: Cross-compile for distribution
release: clean
	@mkdir -p dist
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)_darwin_arm64 .
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)_darwin_amd64 .
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)_linux_arm64 .
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)_linux_amd64 .
	@echo "Binaries in dist/"
	@ls -lh dist/

## warm: Pre-warm tools cache by hitting tools/list on all backends
warm:
	@echo "Warming tools cache..."
	@SPAWNED=0; \
	if ! curl -sf http://127.0.0.1:19900/health >/dev/null 2>&1; then \
		$(PREFIX)/$(BINARY) $(CONFIG_DIR)/config.json &>/dev/null & PID=$$!; \
		SPAWNED=1; \
		for i in 1 2 3 4 5 6 7 8 9 10; do \
			curl -sf http://127.0.0.1:19900/health >/dev/null 2>&1 && break; \
			sleep 0.5; \
		done; \
	fi; \
	for backend in $$(python3 -c "import json,os; d=json.load(open(os.path.expanduser('$(CONFIG_DIR)/backends.json'))); [print(k) for k in d]" 2>/dev/null); do \
		printf "  $$backend: "; \
		RESULT=$$(curl -s --max-time 60 -X POST http://127.0.0.1:19900/$$backend/mcp \
			-H "Content-Type: application/json" \
			-H "Accept: application/json" \
			-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\",\"params\":{}}" 2>/dev/null); \
		TOOLS=$$(echo "$$RESULT" | python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d.get('result',{}).get('tools',[])))" 2>/dev/null || echo "0"); \
		echo "$$TOOLS tools cached"; \
	done; \
	echo "Cache warmed."; \
	if [ "$$SPAWNED" = "1" ]; then kill $$PID 2>/dev/null; fi

## version: Print current version
version:
	@echo $(VERSION)

## tag: Create a new version tag (usage: make tag v=0.2.0)
tag:
	@test -n "$(v)" || (echo "usage: make tag v=0.2.0" && exit 1)
	git tag -a -s v$(v) -m "Release v$(v)"
	@echo "tagged v$(v). Push with: git push origin v$(v)"
