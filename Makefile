BINARY := snoutd
BUILD_DIR := ./build

.PHONY: all build clean run

all: build

build:
	go build -o $(BUILD_DIR)/$(BINARY) .

run: build
	./$(BUILD_DIR)/$(BINARY)

clean:
	rm -rf $(BUILD_DIR)/*