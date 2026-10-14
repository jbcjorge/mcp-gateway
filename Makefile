.PHONY: all build install uninstall clean test test-report fmt vet shadow lint vuln gosec gitleaks cyclomatic cognitive check tools update-deps release cross-compile warm version tag

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

## update-deps: Update direct dependencies + Go to latest, then pin that exact
## Go version everywhere so local tooling (mise/asdf via .tool-versions) and CI
## (go-version-file: go.mod) never float to a different version than go.mod.
## The go directive in go.mod is the single source of truth: Go treats a
## toolchain line equal to it as redundant (go mod tidy strips it), so the pin
## lives in the go directive, mirrored into .tool-versions.
update-deps:
	go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all | tail -n +2 | xargs -I{} go get {}@latest
	go get go@latest
	go mod tidy
	@GOVER=$$(go mod edit -json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Go"])'); \
	echo "go $$GOVER" > .tool-versions; \
	echo "pinned Go $$GOVER in go.mod and .tool-versions"
	go build ./...
	go test -count=1 ./...
	@GOVER=$$(go mod edit -json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Go"])'); \
	TV=$$(awk '/^go /{print $$2}' .tool-versions); \
	test "$$GOVER" = "$$TV" || { echo "PIN MISMATCH: go.mod=$$GOVER .tool-versions=$$TV" >&2; exit 1; }; \
	echo "pin check OK: go.mod == .tool-versions == $$GOVER"

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

## release: Cut a release: update CHANGELOG from commits, commit, sign-tag, push
## (usage: make release VERSION=v1.0.0 MESSAGE="what changed"). The tag push
## triggers the Release workflow, which cross-compiles and publishes artifacts.
release:
ifndef VERSION
	$(error VERSION is required. Usage: make release VERSION=v1.0.0 MESSAGE="what changed")
endif
ifndef MESSAGE
	$(error MESSAGE is required. Usage: make release VERSION=v1.0.0 MESSAGE="what changed")
endif
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "ERROR: VERSION must be semver (e.g. v1.0.0)"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "ERROR: working tree is dirty; commit or stash first."; exit 1; }
	@git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null && { echo "ERROR: tag $(VERSION) already exists."; exit 1; } || true
	@# Build the CHANGELOG section from commits since the last tag. Subjects and
	@# the message go through temp files so metacharacters are never spliced into
	@# the shell or awk program. Merge commits and WIP/release-chore subjects are
	@# excluded. Commit links point at GitHub (/commit/<sha>).
	@prevtag=$$(git describe --tags --abbrev=0 2>/dev/null || true); \
	if [ -n "$$prevtag" ]; then range="$$prevtag..HEAD"; echo "Collecting commits since $$prevtag..."; \
	else range="HEAD"; echo "No previous tag; collecting full history (first release)..."; fi; \
	remote=$$(git remote get-url origin 2>/dev/null); \
	weburl=$$(echo "$$remote" | sed -E \
		-e 's#^ssh://git@([^/:]+)(:[0-9]+)?/#https://\1/#' \
		-e 's#^git@([^:]+):#https://\1/#' \
		-e 's#^([A-Za-z0-9._-]+):([A-Za-z0-9._/-]+)$$#https://\1/\2#' \
		-e 's#\.git$$##'); \
	subjects_file=$$(mktemp); \
	git log $$range --no-merges --invert-grep \
		--grep='^wip' --grep='^fixup!' --grep='^squash!' --grep='^chore(release)' -i \
		--pretty=format:'%H%x09%s' > "$$subjects_file"; printf '\n' >> "$$subjects_file"; \
	if [ "$$(tr -d '[:space:]' < "$$subjects_file")" = "" ]; then \
		echo "ERROR: no releasable commits since $$prevtag; nothing to release."; rm -f "$$subjects_file"; exit 1; fi; \
	msg_file=$$(mktemp); printf '%s' "$(MESSAGE)" > "$$msg_file"; \
	semver=$$(echo "$(VERSION)" | sed 's/^v//'); date=$$(date -u +%Y-%m-%d); \
	echo "Writing CHANGELOG section [$$semver] ($$date)..."; \
	section_file=$$(mktemp); \
	{ printf '## [%s] - %s\n\n' "$$semver" "$$date"; cat "$$msg_file"; printf '\n\n'; \
	  while IFS=$$(printf '\t') read -r sha subject; do [ -n "$$sha" ] || continue; \
	    short=$$(printf '%s' "$$sha" | cut -c1-8); \
	    if [ -n "$$weburl" ]; then printf -- '- %s ([%s](%s/commit/%s))\n' "$$subject" "$$short" "$$weburl" "$$sha"; \
	    else printf -- '- %s (%s)\n' "$$subject" "$$short"; fi; \
	  done < "$$subjects_file"; printf '\n'; } > "$$section_file"; \
	tmp=$$(mktemp); \
	awk -v secfile="$$section_file" 'function emit(l){while((getline l < secfile)>0)print l;close(secfile)} \
		!done && /^## \[/ {emit();done=1} {print} END{if(!done){print "";emit()}}' CHANGELOG.md > "$$tmp" && mv "$$tmp" CHANGELOG.md; \
	prevver=$$(echo "$$prevtag" | sed 's/^v//'); \
	if [ -n "$$weburl" ] && [ -n "$$prevver" ]; then \
	  awk -v nv="$$semver" -v pv="$$prevver" -v url="$$weburl" ' \
	    /^\[Unreleased\]:/ { \
	      printf "[Unreleased]: %s/compare/v%s...HEAD\n", url, nv; \
	      printf "[%s]: %s/compare/v%s...v%s\n", nv, url, pv, nv; \
	      ins=1; next } \
	    {print} \
	    END{if(!ins){printf "[%s]: %s/compare/v%s...v%s\n", nv, url, pv, nv}}' CHANGELOG.md > "$$tmp" && mv "$$tmp" CHANGELOG.md; \
	fi; \
	rm -f "$$subjects_file" "$$msg_file" "$$section_file"
	@echo "Committing and tagging $(VERSION)..."
	git add CHANGELOG.md
	git commit -m "chore(release): $(VERSION)" -m "$(MESSAGE)"
	git tag -a -s $(VERSION) -m "$(MESSAGE)"
	git push origin HEAD
	git push origin $(VERSION)
	@echo "Released $(VERSION). The Release workflow will cross-compile and publish artifacts."

## cross-compile: Cross-compile for distribution (produces dist/ artifacts)
cross-compile: clean
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
