// Package vm wraps Virtualization.framework (via Code-Hex/vz) in the small
// surface thimble needs: build a Linux microVM with the device set every
// phase of the project relies on, start it, watch its state, stop it.
//
// The device list is fixed on purpose. A sandbox is always: virtio console
// (serial), entropy, memory balloon, NAT network, vsock, and optionally one
// block device. Keeping it fixed means a snapshot taken from any guest can be
// restored into any later configuration (phase 3 depends on this: save/restore
// requires an identical device configuration).
package vm

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Code-Hex/vz/v3"
)

type Config struct {
	Kernel  string // uncompressed arm64 Image
	Initrd  string // optional cpio(.gz)
	Cmdline string
	Disk    string // optional raw image attached as virtio-blk (phase 3+)

	CPUs      uint
	MemoryMiB uint64

	// ConsoleIn/ConsoleOut back the guest's hvc0. Either may be nil, in which
	// case that direction is left unattached.
	ConsoleIn, ConsoleOut *os.File
}

type Machine struct {
	cfg Config
	vm  *vz.VirtualMachine
}

// Limits reports the framework's allowed memory range, which is what decides
// how small a sandbox can be on this host.
func Limits() (minMem, maxMem uint64, minCPU, maxCPU uint) {
	return vz.VirtualMachineConfigurationMinimumAllowedMemorySize(),
		vz.VirtualMachineConfigurationMaximumAllowedMemorySize(),
		vz.VirtualMachineConfigurationMinimumAllowedCPUCount(),
		vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
}

func New(cfg Config) (*Machine, error) {
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.MemoryMiB == 0 {
		cfg.MemoryMiB = 256
	}
	opts := []vz.LinuxBootLoaderOption{vz.WithCommandLine(cfg.Cmdline)}
	if cfg.Initrd != "" {
		opts = append(opts, vz.WithInitrd(cfg.Initrd))
	}
	bl, err := vz.NewLinuxBootLoader(cfg.Kernel, opts...)
	if err != nil {
		return nil, fmt.Errorf("bootloader: %w", err)
	}
	vmc, err := vz.NewVirtualMachineConfiguration(bl, cfg.CPUs, cfg.MemoryMiB<<20)
	if err != nil {
		return nil, fmt.Errorf("vm config: %w", err)
	}

	// Serial console -> hvc0 in the guest.
	if cfg.ConsoleIn != nil || cfg.ConsoleOut != nil {
		att, err := vz.NewFileHandleSerialPortAttachment(cfg.ConsoleIn, cfg.ConsoleOut)
		if err != nil {
			return nil, fmt.Errorf("console attachment: %w", err)
		}
		con, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(att)
		if err != nil {
			return nil, fmt.Errorf("console: %w", err)
		}
		vmc.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{con})
	}

	ent, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{ent})

	bal, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmc.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{bal})

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, err
	}
	nic.SetMACAddress(mac)
	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})

	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	if cfg.Disk != "" {
		att, err := vz.NewDiskImageStorageDeviceAttachment(cfg.Disk, false)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", cfg.Disk, err)
		}
		blk, err := vz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		vmc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})
	}

	if ok, err := vmc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid vm configuration: %w", err)
	}
	m, err := vz.NewVirtualMachine(vmc)
	if err != nil {
		return nil, fmt.Errorf("new vm: %w", err)
	}
	return &Machine{cfg: cfg, vm: m}, nil
}

func (m *Machine) Start() error { return m.vm.Start() }

// Stop force-stops the guest (equivalent to pulling the power).
func (m *Machine) Stop() error { return m.vm.Stop() }

func (m *Machine) State() vz.VirtualMachineState { return m.vm.State() }

// Balloon returns the traditional balloon device, nil if absent.
func (m *Machine) Balloon() *vz.VirtioTraditionalMemoryBalloonDevice {
	devs := m.vm.MemoryBalloonDevices()
	if len(devs) == 0 {
		return nil
	}
	return vz.AsVirtioTraditionalMemoryBalloonDevice(devs[0])
}

// Vsock returns the virtio socket device for host->guest connections.
func (m *Machine) Vsock() *vz.VirtioSocketDevice {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil
	}
	return devs[0]
}

// WaitStopped blocks until the VM reaches Stopped or Error, or timeout
// elapses (0 = forever).
func (m *Machine) WaitStopped(timeout time.Duration) error {
	var deadline <-chan time.Time
	if timeout > 0 {
		deadline = time.After(timeout)
	}
	ch := m.vm.StateChangedNotify()
	for {
		switch m.vm.State() {
		case vz.VirtualMachineStateStopped:
			return nil
		case vz.VirtualMachineStateError:
			return errors.New("vm entered error state")
		}
		select {
		case <-ch:
		case <-deadline:
			return errors.New("timeout waiting for vm to stop")
		}
	}
}
