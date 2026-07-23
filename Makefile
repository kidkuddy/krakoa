COMMIT  := $(shell git rev-parse --short HEAD)
LDFLAGS := -ldflags "-X main.buildCommit=$(COMMIT) -X main.buildRepo=$(CURDIR)"

.PHONY: build test vet clean install install-skills run ui

SKILLS_DIR := $(HOME)/.claude/skills

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
install: build install-skills
	cp bin/krakoad $(HOME)/.local/bin/krakoad.tmp && mv -f $(HOME)/.local/bin/krakoad.tmp $(HOME)/.local/bin/krakoad
	cp bin/krakoactl $(HOME)/.local/bin/krakoactl.tmp && mv -f $(HOME)/.local/bin/krakoactl.tmp $(HOME)/.local/bin/krakoactl
	ln -sf $(HOME)/.local/bin/krakoactl /opt/homebrew/bin/krakoactl

# skills/ documents THIS engine — krakoactl's surface and the workflow DSL.
# Living only in ~/.claude/skills, they drifted three weeks behind the code
# (the authoring skill still described the pre-clean-slate Temporal engine).
# The repo is the source of truth; the install is a symlink, so an edit here
# is live and a stale skill shows up as a diff.
install-skills:
	@mkdir -p $(SKILLS_DIR)
	@for s in krakoa krakoa-create-workflow; do \
	  rm -rf "$(SKILLS_DIR)/$$s"; \
	  ln -sfn "$(CURDIR)/skills/$$s" "$(SKILLS_DIR)/$$s"; \
	  echo "linked $(SKILLS_DIR)/$$s -> skills/$$s"; \
	done

run: build
	./bin/krakoad

# rebuild the embedded web UI (node is a build-time dep only; the built
# uidist/ is committed so plain `make build` needs no toolchain)
ui:
	cd ui && npm install && npm run build

clean:
	rm -rf bin
