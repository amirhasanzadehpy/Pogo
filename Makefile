PYTHON ?= python3
GO_GRAMMAR_TAGS := grammar_subset,grammar_subset_python
GO_BUILD_FLAGS := -tags=$(GO_GRAMMAR_TAGS)
GO_TEST_FLAGS := -tags=$(GO_GRAMMAR_TAGS)
FIXTURE_DIR := testdata/sample_django_project
FIXTURE_VENV := .venv-fixture
FIXTURE_PYTHON := $(FIXTURE_VENV)/bin/python
FIXTURE_REQUIREMENTS := $(FIXTURE_DIR)/requirements.txt
FIXTURE_CONSTRAINTS := $(FIXTURE_DIR)/constraints.txt
FIXTURE_STAMP := $(FIXTURE_VENV)/.requirements-installed

ifeq ($(shell uname -s),Darwin)
GO_BUILD_FLAGS += -ldflags=-linkmode=external
GO_TEST_FLAGS += -ldflags=-linkmode=external
endif

.PHONY: all build fixture-env test-env test test-race fuzz bench bench-profile compat release-check clean

all: build

build:
	mkdir -p build
	go build $(GO_BUILD_FLAGS) -o build/pogo ./cmd/pogo
	go build $(GO_BUILD_FLAGS) -o build/testclient ./cmd/testclient

$(FIXTURE_STAMP): $(FIXTURE_REQUIREMENTS) $(FIXTURE_CONSTRAINTS)
	test -x "$(FIXTURE_PYTHON)" || "$(PYTHON)" -m venv "$(FIXTURE_VENV)"
	"$(FIXTURE_PYTHON)" -m pip install -r "$(FIXTURE_REQUIREMENTS)" -c "$(FIXTURE_CONSTRAINTS)"
	touch "$(FIXTURE_STAMP)"

fixture-env: $(FIXTURE_STAMP)
	@REQUESTED=$$("$(PYTHON)" -c 'import sys; print("%s.%s" % sys.version_info[:2])'); INSTALLED=$$("$(FIXTURE_PYTHON)" -c 'import sys; print("%s.%s" % sys.version_info[:2])'); test "$$REQUESTED" = "$$INSTALLED" || { printf 'Fixture environment uses Python %s, but Python %s was requested; run `make clean` first.\n' "$$INSTALLED" "$$REQUESTED" >&2; exit 1; }

test-env: fixture-env
	@"$(FIXTURE_PYTHON)" -c 'import platform; print("Fixture Python:", platform.python_version())'
	@"$(FIXTURE_PYTHON)" -c 'import django; print("Fixture Django:", django.get_version())'
	@$(MAKE) test

test:
	@test -x "$(FIXTURE_PYTHON)" || { printf '%s\n' 'Fixture environment missing; run `make fixture-env` first.' >&2; exit 1; }
	@GO_FILES=$$(git ls-files --cached --others --exclude-standard -- '*.go' | while IFS= read -r FILE; do test ! -f "$$FILE" || printf '%s\n' "$$FILE"; done); if test -n "$$GO_FILES"; then UNFORMATTED=$$(gofmt -l $$GO_FILES); test -z "$$UNFORMATTED" || { printf 'Unformatted Go files:\n%s\n' "$$UNFORMATTED" >&2; exit 1; }; fi
	go vet -tags=$(GO_GRAMMAR_TAGS) ./...
	go test $(GO_TEST_FLAGS) ./...
	PYTHONDONTWRITEBYTECODE=1 "$(FIXTURE_PYTHON)" -m unittest discover -s "$(FIXTURE_DIR)/tests" -p 'test_*.py' -v
	PYTHONDONTWRITEBYTECODE=1 "$(FIXTURE_PYTHON)" -m unittest discover -s src/daemon -p 'test_*.py' -v

test-race:
	go test -race $(GO_TEST_FLAGS) ./...

fuzz:
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzIPCReadFrame$$' -fuzztime=30s ./internal/python
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzParseWorkerEnvironment$$' -fuzztime=30s ./internal/python
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzLSPReadFrame$$' -fuzztime=30s ./internal/harness
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzUTF16PositionRoundTrip$$' -fuzztime=30s ./internal/analysis
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzUTF16EditMatchesFullParse$$' -fuzztime=30s ./internal/analysis
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzORMPathExtraction$$' -fuzztime=30s ./internal/analysis
	go test $(GO_TEST_FLAGS) -run '^$$' -fuzz '^FuzzStoreParserRecovery$$' -fuzztime=30s ./internal/analysis

bench: build
	@"$(PYTHON)" scripts/bench.py

bench-profile: bench

compat:
	@"$(PYTHON)" scripts/compat.py

release-check: build
	@"$(PYTHON)" scripts/check_release.py build/pogo build/testclient

clean:
	rm -rf "$(FIXTURE_VENV)" build bin benchmark-results
	rm -f "$(FIXTURE_DIR)/db.sqlite3" "$(FIXTURE_DIR)"/db.sqlite3-* *.prof *.trace *.sock
