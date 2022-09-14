BUILD_DIR := $(PWD)/.build
SHELL := /bin/bash
UNAME := $(shell uname)
ARCH := $(shell uname -m)
GOARCH := $(ARCH)

ifeq ($(findstring x86_64,$(ARCH)),x86_64)
    GOARCH := amd64
endif

GO_SOURCES := $(shell find `echo *` -name '*.go')

GOLANG_VERSION := 1.18
GOLANGCI_LINT_VERSION := 1.45.2

ifeq ($(findstring MINGW64_NT,$(UNAME)),MINGW64_NT)
    OS := windows
else ifeq ($(findstring Darwin,$(UNAME)),Darwin)
    OS := darwin
else ifeq ($(findstring Linux,$(UNAME)),Linux)
    OS := linux
else
    $(error unsupported OS $(UNAME))
endif

all: $(BUILD_DIR)/bin/plz

.PHONY: all

clean:
	@rm -rf .build

.PHONY: clean

install: install-runtime

.PHONY: install

#
# Development environment
#

ifeq ($(OS),darwin)
$(BUILD_DIR)/go/bin/go:
	@echo installing go...
	@mkdir -p $(BUILD_DIR)/bin
	@cd $(BUILD_DIR) && curl -Ls https://golang.org/dl/go$(GOLANG_VERSION).$(OS)-$(GOARCH).tar.gz | tar xz
endif

$(BUILD_DIR)/bin/golangci-lint: $(BUILD_DIR)/bin/activate
	@echo 'install golangci-lint...'
	@source $(BUILD_DIR)/bin/activate && \
		cd $(BUILD_DIR) && \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v$(GOLANGCI_LINT_VERSION)

install-runtime: \
    $(BUILD_DIR)/bin/golangci-lint \
    $(BUILD_DIR)/go/bin/go

.PHONY: install-runtime

$(BUILD_DIR)/bin/activate: $(BUILD_DIR)/go/bin/go
	@mkdir -p $(BUILD_DIR)/bin
	@echo 'export PATH=$(BUILD_DIR)/bin:$(BUILD_DIR)/go/bin:$$PATH' > $(BUILD_DIR)/bin/activate
	@# Make pkg directory writeable to make it easier to clean up.
	@# See: https://github.com/golang/go/issues/31481
	@#echo 'export GOFLAGS=-modcacherw' >> $(BUILD_DIR)/bin/activate
	@echo 'export PS1="(plz-cli) $$PS1"' >> $(BUILD_DIR)/bin/activate

$(BUILD_DIR)/bin/plz: $(BUILD_DIR)/bin/activate $(GO_SOURCES)
	@source $(BUILD_DIR)/bin/activate && \
		go build -o $(BUILD_DIR)/bin/plz ./cmd/plz

lint: $(BUILD_DIR)/bin/activate $(BUILD_DIR)/bin/golangci-lint
	@source $(BUILD_DIR)/bin/activate && golangci-lint run

.PHONY: lint
