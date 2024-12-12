# Variables
GO = go
TARGET = crawler.exe
DB_PATH = crawler.db

# Default target
all: build run

# Build the application
build:
	$(GO) build --tags "fts5" -o $(TARGET)

# Run the application
run:
	./$(TARGET)

# Clean up
clean:
	- Remove-Item -Force $(TARGET)
	- Remove-Item -Force $(DB_PATH)

.PHONY: all build run clean

dry:
	$(GO) run --tags "fts5" .
