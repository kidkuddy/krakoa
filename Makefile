COMMIT  := $(shell git rev-parse --short HEAD)
LDFLAGS := -ldflags "-X main.buildCommit=$(COMMIT) -X main.buildRepo=$(CURDIR)"

.PHONY: build test vet clean install run ui

build:
	go build $(LDFLAGS) -o bin/krakoad ./cmd/krakoad
	go build $(LDFLAGS) -o bin/krakoactl ./cmd/krakoactl

test:
	go test ./...

vet:
	go vet ./...

install: build
	cp bin/krakoad bin/krakoactl $(HOME)/.local/bin/

run: build
	./bin/krakoad

# rebuild the embedded web UI (node is a build-time dep only; the built
# uidist/ is committed so plain `make build` needs no toolchain)
ui:
	cd ui && npm install && npm run build

clean:
	rm -rf bin
