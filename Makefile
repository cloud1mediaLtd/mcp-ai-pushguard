.PHONY: all build test check coverage architecture-coverage clean install lint daemon mcp

BINARY_NAME=pushguard
BIN_DIR=bin
GOPATH_BIN=$(shell go env GOPATH)/bin
# Identify the source tree that the compiler actually sees. A daemon can stay
# alive while a developer rebuilds from the same Git commit, so commit alone is
# not enough; nor are Git diffs, because newly added untracked Go files are
# compiled too. Hashing tracked and untracked Go sources plus module metadata
# makes an incompatible local rebuild visible before it can submit a job.
BUILD_COMMIT?=$(shell REV=$$(git rev-parse --short HEAD 2>/dev/null || echo dev); SOURCE=$$(git ls-files --cached --others --exclude-standard -z -- '*.go' go.mod go.sum | xargs -0 shasum | shasum | cut -c1-12); echo $$REV-$$SOURCE)

all: test build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags="-s -w -X main.BuildCommit=$(BUILD_COMMIT)" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/pushguard
	@echo "Build complete: $(BIN_DIR)/$(BINARY_NAME)"

test:
	go test -v -race ./...

# check validates formatting without rewriting the shared working tree.
check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	go vet ./...
	PUSHGUARD_NO_AUTOSPAWN=1 PUSHGUARD_REDIS_ADDR='' go test -race ./...
	$(MAKE) architecture-coverage

# coverage reports every package, including legacy adapters.
coverage:
	PUSHGUARD_NO_AUTOSPAWN=1 PUSHGUARD_REDIS_ADDR='' go test -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	@rm -f coverage.out

# Keep new contracts and use cases above 80% while legacy adapter coverage is
# raised incrementally without hiding it from the repository-wide report.
architecture-coverage:
	@for package in ./internal/verification/application ./internal/verification/domain ./pkg/api; do \
		coverage=$$(go test -cover $$package | awk '/coverage:/{print $$5}'); \
		coverage=$${coverage%%%}; \
		awk -v package="$$package" -v coverage="$$coverage" 'BEGIN { if (coverage + 0 < 80) { printf "%s coverage %.1f%% is below 80%%\\n", package, coverage; exit 1 } }'; \
	done

# Installed by atomic rename, never by copying over the existing file.
#
# `cp` writes through the same inode. On macOS that invalidates the kernel's
# cached code signature for a binary that is currently mapped, and the next
# exec of it is SIGKILLed -- exit 137, no message, nothing in any log. Since
# the daemon is long-running and usually mid-gate when someone rebuilds, the
# symptom is `pushguard list` dying instantly right after an install that
# reported success, which reads as PushGuard being broken rather than as an
# install that needs redoing.
#
# Renaming onto the target swaps in a new inode instead: the running daemon
# keeps the file it started from, and every new exec gets an intact one.
install: build
	@mkdir -p $(GOPATH_BIN)
	@cp $(BIN_DIR)/$(BINARY_NAME) $(GOPATH_BIN)/.$(BINARY_NAME).new
	@mv -f $(GOPATH_BIN)/.$(BINARY_NAME).new $(GOPATH_BIN)/$(BINARY_NAME)
	@echo "Installed $(BINARY_NAME) to $(GOPATH_BIN)/$(BINARY_NAME)"
	@echo "The running daemon keeps its old binary; restart it when the queue is idle to pick this up."

daemon: build
	./$(BIN_DIR)/$(BINARY_NAME) daemon

mcp: build
	./$(BIN_DIR)/$(BINARY_NAME) mcp

clean:
	rm -rf $(BIN_DIR) pushguard
