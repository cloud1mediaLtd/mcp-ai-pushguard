.PHONY: all build test clean install lint daemon mcp

BINARY_NAME=pushguard
BIN_DIR=bin
GOPATH_BIN=$(shell go env GOPATH)/bin

all: test build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/pushguard
	@echo "Build complete: $(BIN_DIR)/$(BINARY_NAME)"

test:
	go test -v -race ./...

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
