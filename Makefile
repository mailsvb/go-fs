BINARY  := go-fs
DIST    := dist

# VERSION is the released version and lives in ./VERSION so it is bumped in one
# place. Override it on the command line for a one off build, e.g. from CI:
#   make release VERSION=1.0
VERSION     ?= $(strip $(shell cat $(CURDIR)/VERSION))
STAMP       := $(shell date +%Y%m%d-%H%M%S)
DEV_VERSION := $(VERSION)-dev-$(STAMP)

# CGO_ENABLED=0 selects the pure Go resolver and links nothing from the host.
# On Linux and Windows the result is a fully static binary; on macOS it still
# links libSystem, which is what a static build means there.
GOFLAGS  := -trimpath
BUILDENV := CGO_ENABLED=0

# the platforms a release is built for: linux/amd64, which the rpm packages,
# darwin/arm64, which ships as a plain binary, and windows/amd64, which the
# msi packages
PLATFORMS := linux/amd64 darwin/arm64 windows/amd64

# crosscompile builds go-fs for every platform in PLATFORMS at the version
# passed in. build and release differ only in that version. The Windows
# binary is the one the msi installs, stamped as such, so that its .update
# keeps an msi install an msi install.
define crosscompile
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=$(DIST)/$(BINARY)_$(1)_$${os}_$${arch}; stamp=; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; stamp="-X main.packaging=msi"; fi; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch $(BUILDENV) go build $(GOFLAGS) \
			-ldflags "-s -w -X main.version=$(1) $$stamp" -o $$out . || exit 1; \
	done
	@$(MAKE) --no-print-directory checksums
endef

# The installers ship binaries of their own, stamped with the installer that
# owns them so that they leave updating to it and, at the first start, write
# the starter configuration of that installer, which serves from a folder of
# the platform. They are built into PKG; the packages themselves land in DIST
# beside the release binaries.
PKG  := build/pkg
NFPM := go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0

.PHONY: all build test race cover fuzz vet fmt lint clean release sign checksums version \
	packages rpm msi-inputs docker-inputs docker viewer-deps viewer-update

all: vet test build

# viewer-deps fetches the libraries of the document viewer from npm into
# internal/httpd/viewer/libs, where the binary embeds them, at the versions
# internal/httpd/viewer/libraries.lock pins and checked against the sha512 it
# pins. Only the lock file is committed; a fetch that is already there costs
# nothing. Every target that builds or tests the binary runs it first, so
# that no build is made without the viewer by mistake.
viewer-deps:
	go run ./tools/viewer

# viewer-update pins the latest version of each library, then fetches it.
viewer-update:
	go run ./tools/viewer -update

# build produces the same set of binaries as release, stamped with the dev
# version so a local build always says when it was made.
build: clean viewer-deps
	$(call crosscompile,$(DEV_VERSION))

test: viewer-deps
	go test ./... -count=1

race: viewer-deps
	go test ./... -race -count=1

# cover is race with a coverage profile, whose total is printed last.
cover: viewer-deps
	go test ./... -race -count=1 -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1

# fuzz runs every Fuzz target in the module for FUZZTIME each. go test takes
# one target per run, so they are found and run one after the other. Their
# seed corpora already run as part of test.
FUZZTIME ?= 20s
fuzz:
	@set -e; for file in $$(grep -rl --include='*_test.go' '^func Fuzz' .); do \
		dir=./$$(dirname $${file#./}); \
		for fn in $$(grep -oE '^func Fuzz[A-Za-z0-9_]+' $$file | cut -d' ' -f2); do \
			echo "fuzzing $$dir $$fn for $(FUZZTIME)"; \
			go test $$dir -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME); \
		done; \
	done

vet: viewer-deps
	go vet ./...

fmt:
	gofmt -l -w .

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt found unformatted files:"; gofmt -l .; exit 1)

version:
	@echo $(VERSION)

clean:
	rm -rf $(DIST) build $(BINARY) coverage.out

# release cross compiles every supported platform at the released version.
# With the private key in GOFS_SIGNING_KEY it writes the signed .update files
# and keeps those: a release ships packages and updates rather than plain
# binaries, but for that of darwin, which has no installer and so is the
# download for macOS. Without the key the binaries stay; the release workflow
# insists on it.
release: clean viewer-deps
	$(call crosscompile,$(VERSION))
	@if [ -n "$$GOFS_SIGNING_KEY" ]; then \
		$(MAKE) --no-print-directory sign; \
		for bin in $(DIST)/$(BINARY)_*; do \
			case $$bin in *.update|*_darwin_*) ;; *) rm -f $$bin;; esac; \
		done; \
		$(MAKE) --no-print-directory checksums; \
	else \
		echo "GOFS_SIGNING_KEY is not set, no .update files were written"; \
	fi

# sign appends the update signature to every binary in dist, writing the
# .update file go-fs accepts over HTTP next to each one, and rewrites the
# checksums to cover them. The private key is read from GOFS_SIGNING_KEY.
sign:
	@test -n "$$GOFS_SIGNING_KEY" || (echo "GOFS_SIGNING_KEY is not set"; exit 1)
	@for bin in $(DIST)/$(BINARY)_*; do \
		case $$bin in *.update) continue;; esac; \
		go run ./tools/sign -in $$bin -out $$bin.update || exit 1; \
	done
	@$(MAKE) --no-print-directory checksums

checksums:
	@cd $(DIST) && (sha256sum * > SHA256SUMS 2>/dev/null || shasum -a 256 * > SHA256SUMS)
	@echo "wrote $(DIST)/SHA256SUMS"

# packages builds what the installers need at the released version: the rpm
# packages into DIST, and the binaries and the configuration the msi is made of,
# which only the WiX Toolset on Windows can build, into PKG. It runs after
# release, which empties DIST, and writes the checksums again.
packages: rpm msi-inputs
	@$(MAKE) --no-print-directory checksums

# nfpm takes its files from fixed paths, see packaging/linux/nfpm.yaml. With
# SIGNING_KEY_FILE naming an armored PGP private key, and NFPM_RPM_PASSPHRASE
# unlocking it, the rpm is signed, as the package repository needs it to be.
# It ships the starter configuration as /usr/share/go-fs/go-fs.toml, which its
# first install copies to /var/lib/go-fs/go-fs.toml.
rpm: viewer-deps
	@mkdir -p $(PKG)/linux $(DIST)
	@go run ./tools/starter -installer rpm -o $(PKG)/linux/go-fs.toml
	@echo "building $(PKG)/linux/$(BINARY)"
	@GOOS=linux GOARCH=amd64 $(BUILDENV) go build $(GOFLAGS) \
		-ldflags "-s -w -X main.version=$(VERSION) -X main.packaging=rpm" \
		-o $(PKG)/linux/$(BINARY) .
	@VERSION=$(VERSION) ARCH=amd64 $(NFPM) package --config packaging/linux/nfpm.yaml \
		--packager rpm --target $(DIST)/

# the msi installs the binary and the starter configuration, as
# C:\ProgramData\go-fs\go-fs.toml
msi-inputs: viewer-deps
	@mkdir -p $(PKG)
	@go run ./tools/starter -installer msi -o $(PKG)/go-fs.msi.toml
	@echo "building $(PKG)/$(BINARY)_windows_amd64.exe"
	@GOOS=windows GOARCH=amd64 $(BUILDENV) go build $(GOFLAGS) \
		-ldflags "-s -w -X main.version=$(VERSION) -X main.packaging=msi" \
		-o $(PKG)/$(BINARY)_windows_amd64.exe .

# What packaging/docker/Dockerfile builds the container image from, and all it
# sees: the linux/amd64 binary stamped as the image's, so that it leaves
# updating to a newer image and writes the image's starter configuration, with
# every server listening on all interfaces, to /var/lib/go-fs/go-fs.toml at the
# first start, the entrypoint and the licence.
DOCKER := $(PKG)/docker

docker-inputs: viewer-deps
	@mkdir -p $(DOCKER)
	@echo "building $(DOCKER)/$(BINARY)"
	@GOOS=linux GOARCH=amd64 $(BUILDENV) go build $(GOFLAGS) \
		-ldflags "-s -w -X main.version=$(VERSION) -X main.packaging=docker" \
		-o $(DOCKER)/$(BINARY) .
	@cp packaging/docker/entrypoint.sh LICENSE $(DOCKER)/

# docker builds the image locally as go-fs:VERSION; the release workflow
# builds and pushes it itself from the same inputs.
docker: docker-inputs
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) \
		-f packaging/docker/Dockerfile -t $(BINARY):$(VERSION) $(DOCKER)
