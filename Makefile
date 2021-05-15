BUILD_DIR := $(PWD)/.build
SHELL := /bin/bash
UNAME := $(shell uname)

# Current version of the software.
VERSION := $(shell cat VERSION)

# Platforms to build distributions for.
ALL_DISTS = darwin-amd64 darwin-arm64 linux-amd64

GO_SOURCES := $(shell find `echo *` -name '*.go')

ifeq ($(findstring Darwin,$(UNAME)),Darwin)
    OS := darwin
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
	@cd $(BUILD_DIR) && curl -Ls https://golang.org/dl/go1.16.4.darwin-amd64.tar.gz | tar xz
endif

$(BUILD_DIR)/bin/golangci-lint: $(BUILD_DIR)/bin/activate
	@echo 'install golangci-lint...'
	@source $(BUILD_DIR)/bin/activate && \
		cd $(BUILD_DIR) && \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s v1.31.0

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
	@source $(BUILD_DIR)/bin/activate && \
		golangci-lint run $(GO_SOURCES)

.PHONY: lint

#
# Distribution
#

ALL_DIST_FILES := $(foreach dist,$(ALL_DISTS),$(BUILD_DIR)/dist/plz-$(dist)-$(VERSION).tar.gz)

$(BUILD_DIR)/dist/%/plz: $(BUILD_DIR)/bin/activate $(GO_SOURCES) VERSION
	@source $(BUILD_DIR)/bin/activate && \
		GOOS=`echo $* | awk -F- '{ print $$1 }'` \
		GOARCH=`echo $* | awk -F- '{ print $$2 }'` \
		go build -ldflags "-X main.Version=$(VERSION)" -o $@ ./cmd/plz

$(BUILD_DIR)/dist/plz-%-$(VERSION).tar.gz: $(BUILD_DIR)/dist/%/plz
	@tar -czf $@ -C $(<D) plz

# Explicitly list dist binaries so that they're not marked as intermediate files
# and removed:
# https://www.gnu.org/software/make/manual/html_node/Chained-Rules.html#Chained-Rules
dist-bins: $(foreach dist,$(ALL_DISTS),$(BUILD_DIR)/dist/$(dist)/plz)

.PHONY: dist-bins

release: $(ALL_DIST_FILES)
	@gh release create $(VERSION) $(ALL_DIST_FILES) --title $(VERSION) --notes ""

.PHONY: release
