PLUGIN_ID ?= rocks.sherwin.mention-notifier
BINARY ?= plugin
VERSION ?= 0.2.0
GO ?= go
GOFLAGS ?=
DATA_FILE ?= data.csv

.PHONY: build test fmt vet check package clean

build:
	$(GO) $(GOFLAGS) build -trimpath -o $(BINARY) .

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

check: fmt test vet

package: build
	@test -f "$(DATA_FILE)" || (echo "ERROR: $(DATA_FILE) is required to package the plugin" >&2; exit 1)
	rm -f "$(PLUGIN_ID)-$(VERSION).tar.gz"
	tar -czf "$(PLUGIN_ID)-$(VERSION).tar.gz" \
		"$(BINARY)" \
		plugin.json \
		"$(DATA_FILE)"

clean:
	rm -f "$(BINARY)" "$(PLUGIN_ID)-$(VERSION).tar.gz"
