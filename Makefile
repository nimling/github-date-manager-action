# Makefile for the date-manager application
# This file contains targets for local development and Docker deployment

# Default target to list all available commands
.PHONY: help
help:
	@echo "Available commands:"
	@echo "  make dev          - Build and run application locally"
	@echo "  make docker-build - Build and run application in Docker container"
	@echo "  make clean        - Remove build artifacts"

# Build and run the app locally
.PHONY: dev
dev:
	@echo "Building date-manager..."
	@go build -o date-manager
	@echo "Running date-manager locally..."
	@./date-manager

# Build Docker image and run container with environment variables
.PHONY: docker-build
docker-build:
	@echo "Building Docker image..."
	@docker build -t date-manager .
	@echo "Running Docker container with environment variables..."
	@docker run -it --env-file .env date-manager

# Clean build artifacts
.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	@rm -f date-manager
	@echo "Done!"