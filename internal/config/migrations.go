package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const CurrentVersion = 1

type migrateRule struct {
	Version int
	Apply   func(cfg *Config) error
}

var migrateRules = []migrateRule{
	{Version: 1, Apply: func(cfg *Config) error {
		if cfg.PermissionMode == "" {
			cfg.PermissionMode = "default"
		}
		return nil
	}},
}

func Migrate(cfg *Config, fromVersion int) error {
	if fromVersion < 0 {
		fromVersion = 0
	}
	if fromVersion >= CurrentVersion {
		return nil
	}

	// `before := *cfg` is a SHALLOW copy: Config holds maps and pointers
	// (MCPServers, Profiles, MemoryEmbedding, DoneVerifyCommands), so a
	// migration that mutates one of those in place mutates `before` too and
	// DeepEqual then compares the post-migration value against itself —
	// reporting "unchanged" and skipping the Save, which loses the migration.
	// Serializing the pre-state sidesteps the aliasing entirely.
	beforeJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("snapshot config before migration: %w", err)
	}

	changed := false
	for _, m := range migrateRules {
		if m.Version > fromVersion {
			if err := m.Apply(cfg); err != nil {
				return fmt.Errorf("migration v%d: %w", m.Version, err)
			}
		}
	}
	afterJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("snapshot config after migration: %w", err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		changed = true
	}
	if !changed {
		return nil
	}
	return Save(cfg)
}
