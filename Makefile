# thimble: tiny Linux microVMs on Apple Silicon via Virtualization.framework.
#
# The one non-obvious step is codesigning. Virtualization.framework refuses to
# start a VM from a binary that lacks the com.apple.security.virtualization
# entitlement, and `go build` produces an unsigned binary. An ad-hoc signature
# (`-s -`) carrying entitlements.plist is enough; no developer account needed.
# This also means `go run ./cmd/thimble` can never work. Use `make boot`.

GO      ?= go
BIN     := bin/thimble
ALPINE_VERSION := 3.22
ALPINE_RELEASE := 3.22.6
KERNEL_PKG     := linux-virt-6.12.111-r0
MIRROR  := https://dl-cdn.alpinelinux.org/alpine/v$(ALPINE_VERSION)

ALPINE_DIR := build/alpine
ROOTFS_TAR := $(ALPINE_DIR)/alpine-minirootfs-$(ALPINE_RELEASE)-aarch64.tar.gz
KERNEL_APK := $(ALPINE_DIR)/$(KERNEL_PKG).apk
GUEST_DIR  := build/guest
KERNEL     := $(GUEST_DIR)/vmlinux
INITRD     := $(GUEST_DIR)/initramfs.cpio.gz

MEM  ?= 256
CPUS ?= 1

.PHONY: all build image fetch boot bench run snapshot restore serve sdk-venv sdk-test clean distclean

all: build image

build: $(BIN)

$(BIN): $(shell find cmd internal -name '*.go') go.mod go.sum entitlements.plist
	@mkdir -p bin
	$(GO) build -o $@ ./cmd/thimble
	codesign --force --sign - --entitlements entitlements.plist $@
	@codesign -d --entitlements - $@ 2>&1 | grep -q com.apple.security.virtualization && echo "signed: $@ (ad-hoc, virtualization entitlement)"

fetch: $(ROOTFS_TAR) $(KERNEL_APK)

$(ROOTFS_TAR):
	@mkdir -p $(ALPINE_DIR)
	curl -fsSL -o $@ $(MIRROR)/releases/aarch64/$(notdir $@)

# Guest userspace additions, layered over the Alpine minirootfs by mkimage.
ENVD_SRC ?= ../runtime/packages/envd
OVERLAY  := build/overlay
ENVD     := $(OVERLAY)/usr/bin/envd
VSOCKFWD := $(OVERLAY)/usr/bin/vsockfwd
GUEST_GO := CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -buildvcs=false -ldflags "-s -w"

# envd is E2B's agent, reused unchanged and cross-compiled for linux/arm64.
$(ENVD): $(shell find $(ENVD_SRC) -name '*.go' -not -name '*_test.go' 2>/dev/null)
	@mkdir -p $(dir $@)
	cd $(ENVD_SRC) && $(GUEST_GO) -ldflags "-s -w -X=github.com/e2b-dev/infra/packages/envd/pkg.Version=0.9.0" -o $(abspath $@) .

$(VSOCKFWD): cmd/vsockfwd/main.go
	@mkdir -p $(dir $@)
	$(GUEST_GO) -o $@ ./cmd/vsockfwd

# Extra Alpine packages unpacked into the rootfs. bash is non-negotiable: the
# E2B SDKs run every command as `/bin/bash -l -c`. Each is a plain tarball;
# mkimage unpacks them without apk (no install scripts, fine for these).
EXTRA_PKGS := bash-5.2.37-r0 readline-8.2.13-r1 libncursesw-6.5_p20250503-r0 ncurses-terminfo-base-6.5_p20250503-r0
EXTRA_APKS := $(addprefix $(ALPINE_DIR)/,$(addsuffix .apk,$(EXTRA_PKGS)))

$(ALPINE_DIR)/%.apk:
	@mkdir -p $(ALPINE_DIR)
	curl -fsSL -o $@ $(MIRROR)/main/aarch64/$(notdir $@)

image: $(KERNEL) $(INITRD)

$(KERNEL) $(INITRD): $(ROOTFS_TAR) $(KERNEL_APK) $(EXTRA_APKS) $(ENVD) $(VSOCKFWD) cmd/mkimage/main.go internal/cpio/writer.go
	$(GO) run ./cmd/mkimage -rootfs $(ROOTFS_TAR) -kernel-apk $(KERNEL_APK) -overlay $(OVERLAY) -out $(GUEST_DIR) \
		-apks $(subst $(eval) ,$(comma),$(EXTRA_APKS))
comma := ,

# Interactive: serial console on your terminal. Ctrl-] detaches and kills the VM.
boot: build image
	./$(BIN) boot -kernel $(KERNEL) -initrd $(INITRD) -mem $(MEM) -cpus $(CPUS)

# Non-interactive: boot, wait for the ready marker, print timings and memory, stop.
bench: build image
	./$(BIN) boot -kernel $(KERNEL) -initrd $(INITRD) -mem $(MEM) -cpus $(CPUS) -bench

# Phase 2: boot, reach envd over vsock, run a command through its process service.
run: build image
	./$(BIN) run -kernel $(KERNEL) -initrd $(INITRD) -mem $(MEM) -cpus $(CPUS) -- $(or $(CMD),echo hello from envd)

# Phase 3: save a booted, envd-ready guest; then create sandboxes by restoring it.
SNAP ?= build/snap
snapshot: build image
	./$(BIN) snapshot -kernel $(KERNEL) -initrd $(INITRD) -mem $(MEM) -cpus $(CPUS) -out $(SNAP)

restore: build
	./$(BIN) restore -snapshot $(SNAP) -n $(or $(N),1,5,10)

# Phase 4+5: the E2B-compatible control plane. Point the stock SDK at it:
#   E2B_API_KEY=anything E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983
serve: build
	./$(BIN) serve -snapshot $(SNAP)

# Run the E2B Python SDK's sandbox tests against a running `make serve`.
# Needs a venv with the SDK: make sdk-venv (uses python3.13 from Homebrew).
E2B_SDK ?= ../E2B/packages/python-sdk
VENV    := build/venv
sdk-venv:
	python3.13 -m venv $(VENV)
	$(VENV)/bin/pip install -q -e $(E2B_SDK) 'pytest>=9,<10' 'pytest-asyncio>=1.3,<2' 'pytest-timeout>=2.4,<3' 'pytest-xdist>=3.3,<4' 'pytest-dotenv>=0.5.2,<0.6'

SDK_TESTS ?= tests/sync/sandbox_sync/test_create.py tests/sync/sandbox_sync/test_connect.py tests/sync/sandbox_sync/test_kill.py tests/sync/sandbox_sync/test_timeout.py tests/sync/sandbox_sync/commands tests/sync/sandbox_sync/files
sdk-test:
	cd $(E2B_SDK) && env -u E2B_DEBUG E2B_API_KEY=test E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983 \
		$(abspath $(VENV))/bin/python -m pytest -p no:cacheprovider --timeout 120 -q -rfE $(SDK_TESTS)

clean:
	rm -rf bin $(GUEST_DIR) $(OVERLAY) $(SNAP)

distclean: clean
	rm -rf build
