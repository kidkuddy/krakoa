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

# /opt/homebrew/bin is on every clean macOS PATH (Slack-spawned agent
# sessions don't source the user's shell profile) — symlink so krakoactl
# resolves everywhere, not just in dotfile-blessed shells.
# Copy to a temp path and rename: overwriting a binary in place while launchd
# is executing it leaves a half-mapped image, and the restart then dies instead
# of coming back (found live — krakoad stayed down after an install).
install: build
	cp bin/krakoad $(HOME)/.local/bin/krakoad.tmp && mv -f $(HOME)/.local/bin/krakoad.tmp $(HOME)/.local/bin/krakoad
	cp bin/krakoactl $(HOME)/.local/bin/krakoactl.tmp && mv -f $(HOME)/.local/bin/krakoactl.tmp $(HOME)/.local/bin/krakoactl
	ln -sf $(HOME)/.local/bin/krakoactl /opt/homebrew/bin/krakoactl

run: build
	./bin/krakoad

# rebuild the embedded web UI (node is a build-time dep only; the built
# uidist/ is committed so plain `make build` needs no toolchain)
ui:
	cd ui && npm install && npm run build

clean:
	rm -rf bin
