.PHONY: all build test clean install lint daemon mcp

BINARY_NAME=pushguard
BIN_DIR=bin

all: test build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/pushguard
	@echo "Build complete: $(BIN_DIR)/$(BINARY_NAME)"

test:
	go test -v -race ./...

install: build
	@cp $(BIN_DIR)/$(BINARY_NAME) $(GOPATH)/bin/$(BINARY_NAME) 2>/dev/null || cp $(BIN_DIR)/$(BINARY_NAME) /usr/local/bin/$(BINARY_NAME) 2>/dev/null || (echo "Please copy $(BIN_DIR)/$(BINARY_NAME) to your PATH" && exit 1)
	@echo "Installed $(BINARY_NAME) to PATH"

daemon: build
	./$(BIN_DIR)/$(BINARY_NAME) daemon

mcp: build
	./$(BIN_DIR)/$(BINARY_NAME) mcp

clean:
	rm -rf $(BIN_DIR) pushguard
