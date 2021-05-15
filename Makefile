SHELL := /bin/bash

UNAME := $(shell uname)

BUILD_DIR := $(PWD)/build

ifeq ($(findstring Darwin,$(UNAME)),Darwin)
    OS := darwin
else
    $(error unsupported OS $(UNAME))
endif

ifeq ($(OS),darwin)
$(BUILD_DIR)/go/bin/go:
	@echo installing go...
	@mkdir -p $(BUILD_DIR)/bin
	@cd $(BUILD_DIR) && curl -Ls https://golang.org/dl/go1.15.1.darwin-amd64.tar.gz | tar xz
endif

$(BUILD_DIR)/bin/golangci-lint: $(BUILD_DIR)/bin/activate
	@echo 'install golangci-lint...'
	@source $(BUILD_DIR)/bin/activate && cd build && curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v1.31.0

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

install: install-runtime

.PHONY: install

$(BUILD_DIR)/bin/plz: $(BUILD_DIR)/bin/activate
	@source $(BUILD_DIR)/bin/activate && go build -o $(BUILD_DIR)/bin/plz ./cmd/plz

all: $(BUILD_DIR)/bin/plz

.PHONY: all

clean:
	@rm -rf build

.PHONY: clean

lint: $(BUILD_DIR)/bin/activate $(BUILD_DIR)/bin/golangci-lint
	@golangci-lint run ./actions/... ./cmd/... ./deps/...

$(BUILD_DIR)/dist/version: $(BUILD_DIR)/bin/plz
	@mkdir -p $(BUILD_DIR)/dist
	@$(BUILD_DIR)/bin/plz --version | awk '{print $$3}' > $(BUILD_DIR)/dist/version

release: $(BUILD_DIR)/bin/activate $(BUILD_DIR)/bin/plz $(BUILD_DIR)/dist/version
	@export version=`cat $(BUILD_DIR)/dist/version` && \
		tar -czf $(BUILD_DIR)/dist/plz-$$version.tar.gz -C $(BUILD_DIR)/bin plz && \
		gh release create $$version $(BUILD_DIR)/dist/plz-$$version.tar.gz --title $$version --notes ""

.PHONY: lint
