// Package snapshot describes a saved, envd-ready guest: the framework state
// file plus the configuration a restore must reproduce exactly.
package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const StateFile = "state.vzvmstate"

// Meta is written next to the state file. restoreMachineStateFromURL
// rejects the file unless the VM configuration is identical to the one it was
// saved from, so everything that went into vm.Config is here, including the
// two values the framework would otherwise randomise: the MAC and the generic
// platform machine identifier.
type Meta struct {
	Kernel      string    `json:"kernel"`
	Initrd      string    `json:"initrd"`
	Cmdline     string    `json:"cmdline"`
	MemMiB      uint64    `json:"memMiB"`
	CPUs        uint      `json:"cpus"`
	MAC         string    `json:"mac"`
	MachineID   []byte    `json:"machineId"`
	EnvdVersion string    `json:"envdVersion"`
	Created     time.Time `json:"created"`
}

func Load(dir string) (Meta, string, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		return m, "", err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, "", err
	}
	state, _ := filepath.Abs(filepath.Join(dir, StateFile))
	return m, state, nil
}

func (m Meta) Save(dir string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(dir, "snapshot.json"), b, 0o644)
}
