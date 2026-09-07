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

install: build
	@mkdir -p $(GOPATH_BIN)
	@cp $(BIN_DIR)/$(BINARY_NAME) $(GOPATH_BIN)/$(BINARY_NAME)
	@echo "Installed $(BINARY_NAME) to $(GOPATH_BIN)/$(BINARY_NAME)"

daemon: build
	./$(BIN_DIR)/$(BINARY_NAME) daemon

mcp: build
	./$(BIN_DIR)/$(BINARY_NAME) mcp

clean:
	rm -rf $(BIN_DIR) pushguard
