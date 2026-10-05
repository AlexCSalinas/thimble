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

.PHONY: all build image fetch boot bench clean distclean

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

$(KERNEL_APK):
	@mkdir -p $(ALPINE_DIR)
	curl -fsSL -o $@ $(MIRROR)/main/aarch64/$(notdir $@)

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

image: $(KERNEL) $(INITRD)

$(KERNEL) $(INITRD): $(ROOTFS_TAR) $(KERNEL_APK) $(ENVD) $(VSOCKFWD) cmd/mkimage/main.go internal/cpio/writer.go
	$(GO) run ./cmd/mkimage -rootfs $(ROOTFS_TAR) -kernel-apk $(KERNEL_APK) -overlay $(OVERLAY) -out $(GUEST_DIR)

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

clean:
	rm -rf bin $(GUEST_DIR) $(OVERLAY) $(SNAP)

distclean: clean
	rm -rf build
