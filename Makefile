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
# Full guest in RAM (boot/run/snapshot/mkdisk); boot initramfs (modules +
# switch_root, for disk-backed sandboxes); template root disk (sandboxes boot
# APFS clones of it). Comments stay on their own lines: make keeps the
# whitespace before an inline comment as part of the value.
INITRD      := $(GUEST_DIR)/initramfs.cpio.gz
BOOT_INITRD := $(GUEST_DIR)/boot.cpio.gz
ROOTFS_IMG  := $(GUEST_DIR)/rootfs.img

MEM  ?= 256
CPUS ?= 1

# Template disk: size is sparse (only written blocks cost space), packages are
# installed by apk inside the guest. Change GUEST_PKGS, then `make disk` after
# removing build/guest/rootfs.img.
DISK_MB     ?= 4096
SANDBOX_MEM ?= 512
# E2B_API_URL=http://localhost:$(API_PORT), E2B_SANDBOX_URL=http://localhost:$(ENVD_PORT)
API_PORT    ?= 3000
ENVD_PORT   ?= 49983
SDK_ENV      = E2B_API_KEY=test E2B_API_URL=http://localhost:$(API_PORT) E2B_SANDBOX_URL=http://localhost:$(ENVD_PORT)
GUEST_PKGS  ?= bash coreutils findutils grep sed gawk diffutils tar gzip xz procps-ng \
               ca-certificates curl wget git openssh-client python3 py3-pip nodejs npm \
               sudo jq less file make

.PHONY: all build image disk fetch boot bench run snapshot restore serve smoke sdk-venv sdk-test clean distclean

all: build image disk

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

image: $(KERNEL) $(INITRD) $(BOOT_INITRD)

$(KERNEL) $(INITRD) $(BOOT_INITRD): $(ROOTFS_TAR) $(KERNEL_APK) $(EXTRA_APKS) $(ENVD) $(VSOCKFWD) cmd/mkimage/main.go internal/cpio/writer.go
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

# Phase 6: the template disk. Boots the full initramfs with a blank image
# attached; the guest formats it, copies itself in and apk adds GUEST_PKGS.
# Needs network. ~10 s.
disk: $(ROOTFS_IMG)

$(ROOTFS_IMG): $(BIN) $(KERNEL) $(INITRD)
	./$(BIN) mkdisk -kernel $(KERNEL) -initrd $(INITRD) -out $@ -size $(DISK_MB) -pkgs "$(GUEST_PKGS)"

# Phase 4-6: the E2B-compatible control plane. Point the stock SDK at it:
#   E2B_API_KEY=anything E2B_API_URL=http://localhost:3000 E2B_SANDBOX_URL=http://localhost:49983
# Port 3000 taken (a Next.js dev server, say)? `make serve smoke API_PORT=3100`.
serve: build image disk
	./$(BIN) serve -kernel $(KERNEL) -initrd $(BOOT_INITRD) -disk $(ROOTFS_IMG) -mem $(SANDBOX_MEM) -cpus $(CPUS) \
		-api 127.0.0.1:$(API_PORT) -envd 127.0.0.1:$(ENVD_PORT)

# End-to-end check of an agent-style workload against a running `make serve`:
# python, pip, git, curl, node, sudo, files, pause/resume.
smoke: sdk-venv-check
	$(SDK_ENV) $(VENV)/bin/python scripts/smoke.py

sdk-venv-check:
	@test -x $(VENV)/bin/python || { echo "no $(VENV): run make sdk-venv first"; exit 1; }

# Run the E2B Python SDK's sandbox tests against a running `make serve`.
# Needs a venv with the SDK: make sdk-venv (uses python3.13 from Homebrew).
E2B_SDK ?= ../E2B/packages/python-sdk
VENV    := build/venv
sdk-venv:
	python3.13 -m venv $(VENV)
	$(VENV)/bin/pip install -q -e $(E2B_SDK) 'pytest>=9,<10' 'pytest-asyncio>=1.3,<2' 'pytest-timeout>=2.4,<3' 'pytest-xdist>=3.3,<4' 'pytest-dotenv>=0.5.2,<0.6'

SDK_TESTS ?= tests/sync/sandbox_sync/test_create.py tests/sync/sandbox_sync/test_connect.py tests/sync/sandbox_sync/test_kill.py tests/sync/sandbox_sync/test_timeout.py tests/sync/sandbox_sync/commands tests/sync/sandbox_sync/files
sdk-test:
	cd $(E2B_SDK) && env -u E2B_DEBUG $(SDK_ENV) \
		$(abspath $(VENV))/bin/python -m pytest -p no:cacheprovider --timeout 120 -q -rfE $(SDK_TESTS)

clean:
	rm -rf bin $(GUEST_DIR) $(OVERLAY) $(SNAP) build/sandboxes

distclean: clean
	rm -rf build
