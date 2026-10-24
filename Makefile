.PHONY: help test test-race cover cover-html fmt fmt-check vet build tidy tidy-all release-prep release-notes tag-modules release-notes-test check

COVER_PROFILE := cover.out

# Tracked files only. Plain `find` also walks gitignored scratch, notably the
# repo copies under .claude/worktrees/.
GO_SOURCES := git ls-files '*.go'

# The tagged sub-modules are listed once, in scripts/sync_module_versions.py.
SUB_MODULES := $(shell python3 scripts/sync_module_versions.py --list)
GO_MODULES := . $(SUB_MODULES)

# Untagged modules: tools for working on Dashi, each with its own go.mod and
# go.work. They are tested and vetted here but never tagged or released.
UNTAGGED_MODULES := devtools/agent-archive

help:
	@echo "Available targets:"
	@echo "  make test        - Run tests in every module"
	@echo "  make test-race   - Run tests with race detector in every module"
	@echo "  make cover       - Run root module tests with coverage profile"
	@echo "  make cover-html  - Open HTML coverage report (writes $(COVER_PROFILE))"
	@echo "  make fmt         - Format all Go files"
	@echo "  make fmt-check   - Fail if Go files need formatting"
	@echo "  make vet         - Run go vet (with and without -tags live) in every module"
	@echo "  make tidy        - Tidy root module dependencies"
	@echo "  make tidy-all    - Tidy and format all modules"
	@echo "  make build       - Build the dashi CLI"
	@echo "  make release-notes-test - Run the release tooling unit tests"
	@echo "  make release-prep VERSION=v0.2.0 - Point sub-module requirements at VERSION"
	@echo "  make release-notes VERSION=v0.2.0 - Draft docs/releases/v0.2.0.md"
	@echo "  make tag-modules VERSION=v0.2.0 - Tag all sub-modules (not the root)"
	@echo "  make check       - Run script tests, fmt-check, vet, and test"

test:
	@for dir in $(GO_MODULES) $(UNTAGGED_MODULES); do \
		echo "==> $$dir"; \
		(cd $$dir && go test ./...) || exit 1; \
	done

test-race:
	@for dir in $(GO_MODULES) $(UNTAGGED_MODULES); do \
		echo "==> $$dir"; \
		(cd $$dir && go test -race ./...) || exit 1; \
	done

cover:
	go test -coverprofile $(COVER_PROFILE) ./...
	go tool cover -func $(COVER_PROFILE)

cover-html:
	go test -coverprofile $(COVER_PROFILE) ./...
	go tool cover -html=$(COVER_PROFILE)

fmt:
	gofmt -w $$($(GO_SOURCES))

fmt-check:
	@test -z "$$(gofmt -l $$($(GO_SOURCES)))" || \
		(echo "The following files need gofmt:" && \
		gofmt -l $$($(GO_SOURCES)) && exit 1)

vet:
	@for dir in $(GO_MODULES) $(UNTAGGED_MODULES); do \
		echo "==> $$dir"; \
		(cd $$dir && go vet ./... && go vet -tags live ./...) || exit 1; \
	done

tidy:
	go mod tidy

tidy-all:
	@for dir in $(GO_MODULES); do \
		echo "==> $$dir"; \
		(cd $$dir && go mod tidy && gofmt -w $$($(GO_SOURCES))) || exit 1; \
	done

build:
	cd cmd/dashi && go build .

release-prep:
ifndef VERSION
	$(error VERSION is required. Usage: make release-prep VERSION=v0.2.0)
endif
	python3 scripts/sync_module_versions.py $(VERSION) --write
	@echo ""
	@echo "Commit the go.mod updates, then run: make tag-modules VERSION=$(VERSION)"

release-notes:
ifndef VERSION
	$(error VERSION is required. Usage: make release-notes VERSION=v0.2.0)
endif
	@python3 scripts/release_notes.py $(VERSION)

# Tags the sub-modules only. The root tag (plain VERSION) is created by hand,
# on the same commit, before this runs.
tag-modules:
ifndef VERSION
	$(error VERSION is required. Usage: make tag-modules VERSION=v0.2.0)
endif
	@python3 scripts/sync_module_versions.py $(VERSION) --check
	@for mod in $(SUB_MODULES); do \
		echo "Tagging $$mod/$(VERSION)"; \
		git tag "$$mod/$(VERSION)" || exit 1; \
	done
	@echo ""
	@echo "Tags created. Push the sub-module tags, then the root tag alone:"
	@echo "  git push origin $(addsuffix /$(VERSION),$(SUB_MODULES))"
	@echo "  git push origin $(VERSION)"

release-notes-test:
	python3 -B -m unittest -v scripts.test_changelog_section scripts.test_release_notes scripts.test_sync_module_versions

check: release-notes-test fmt-check vet test
