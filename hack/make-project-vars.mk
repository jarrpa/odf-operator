PROJECT_DIR := $(PWD)
BIN_DIR := $(PROJECT_DIR)/bin
ENVTEST_ASSETS_DIR := $(PROJECT_DIR)/testbin

GOBIN ?= $(BIN_DIR)
GOOS ?= linux
GOARCH ?= amd64
GOPROXY ?= https://proxy.golang.org/
