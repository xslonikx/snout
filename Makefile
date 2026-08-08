BINARY := snout
BUILD_DIR := build

.PHONY: all build clean run

all: build

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/snout

run: build
	./$(BUILD_DIR)/$(BINARY) -f

clean:
	rm -rf $(BUILD_DIR)
