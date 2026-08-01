PLUGIN_NAME := multimodal-router
DIST_DIR := dist
GO ?= go
VERSION ?=
LDFLAGS ?= -s -w
VERSION_LDFLAGS := $(if $(VERSION),-X main.pluginVersion=$(VERSION),)

.PHONY: test vet build clean

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# Local native build (linux/amd64). Requires cgo + a working C compiler.
build:
	mkdir -p $(DIST_DIR)/linux_amd64
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared \
		-ldflags='$(LDFLAGS) $(VERSION_LDFLAGS)' \
		-o $(DIST_DIR)/linux_amd64/$(PLUGIN_NAME).so .

clean:
	rm -rf $(DIST_DIR)
