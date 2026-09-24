# SPDX-FileCopyrightText: 2026 Nextcloud contributors
# SPDX-License-Identifier: AGPL-3.0-or-later

app_name=task_proc_util
build_dir=$(CURDIR)/build
bin_dir=$(CURDIR)/bin
supervisor_dir=$(CURDIR)/supervisor

# Detect a CI / release build
GO ?= go
NPM ?= npm

.PHONY: all
all: build

.PHONY: build
build: composer-install supervisor-release frontend

.PHONY: composer-install
composer-install:
	composer install --no-dev --optimize-autoloader

.PHONY: frontend
frontend:
	$(NPM) ci
	$(NPM) run build

# Target arches shipped in the app. CGO is disabled so cross-compiling needs no
# C toolchain. Each binary lands in bin/<GOARCH>/ and the bin/ wrapper script
# selects the right one at runtime based on `uname -m`.
RELEASE_TARGETS=linux/amd64 linux/arm64 linux/arm

# Build only for the host arch (fast, for local development).
.PHONY: supervisor
supervisor:
	mkdir -p $(bin_dir)/$(shell $(GO) env GOARCH)
	cd $(supervisor_dir) && CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" \
		-o $(bin_dir)/$(shell $(GO) env GOARCH)/task_proc_util-supervisor ./cmd/supervisor

# Cross-compile a binary for every shipped arch.
.PHONY: supervisor-release
supervisor-release:
	@for target in $(RELEASE_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "Building $$os/$$arch"; \
		mkdir -p $(bin_dir)/$$arch; \
		cd $(supervisor_dir) && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "-s -w" \
			-o $(bin_dir)/$$arch/task_proc_util-supervisor ./cmd/supervisor || exit 1; \
	done

.PHONY: supervisor-test
supervisor-test:
	cd $(supervisor_dir) && $(GO) test ./...

.PHONY: supervisor-vet
supervisor-vet:
	cd $(supervisor_dir) && $(GO) vet ./...

.PHONY: clean
clean:
	# Only the compiled per-arch binaries; bin/task_proc_util-supervisor is
	# tracked source, not build output.
	rm -rf $(build_dir) $(bin_dir)/amd64 $(bin_dir)/arm64 $(bin_dir)/arm
	rm -rf js node_modules vendor

.PHONY: appstore
appstore: clean build
	mkdir -p $(build_dir)/artifacts
	rsync -a --delete \
		--exclude='/.git' \
		--exclude='/.github' \
		--exclude='/build' \
		--exclude='/supervisor' \
		--exclude='/src' \
		--exclude='/node_modules' \
		--exclude='/Makefile' \
		--exclude='/package.json' \
		--exclude='/package-lock.json' \
		--exclude='/vite.config.js' \
		./ $(build_dir)/$(app_name)/
	tar -czf $(build_dir)/artifacts/$(app_name).tar.gz -C $(build_dir) $(app_name)
