// Package cache provides the fireactions runner cache: a bounded,
// disk-persistent HTTP cache component that the server manages alongside
// its existing gRPC and metrics listeners.
package cache

import (
	"fmt"

	units "github.com/docker/go-units"
)

// DefaultDataDir is the default directory where cache contents are stored on disk.
const DefaultDataDir = "/var/lib/fireactions/cache"

// Config is the configuration for the cache component.
//
// CORE-07: this schema must never carry a credential-shaped field (no
// username, password, token, or api key) — the cache serves public content
// only, and Phase 1 has no upstream fetcher to configure by design.
type Config struct {
	Enabled     bool   `yaml:"enabled" validate:""`
	BindAddress string `yaml:"bind_address" validate:"required_if=Enabled true,omitempty,hostname_port"`
	MaxSize     string `yaml:"max_size" validate:"required_if=Enabled true"`
	DataDir     string `yaml:"data_dir" validate:""`
}

// DefaultConfig creates a new Config with default values.
func DefaultConfig() *Config {
	return &Config{
		Enabled:     false,
		BindAddress: "",
		MaxSize:     "",
		DataDir:     DefaultDataDir,
	}
}

// MaxSizeBytes parses MaxSize into a byte count.
//
// This uses Docker-CLI semantics (units.RAMInBytes): every size suffix is
// binary regardless of spelling, so "50GB", "50GiB" and "50G" all parse to
// the same value (powers of 1024), and a bare number is a raw byte count.
// go-units fixes the base per function, not per suffix, so there is no way
// to honor both a decimal and a binary convention from a single call site —
// do not "fix" this into a decimal/binary split.
func (c *Config) MaxSizeBytes() (int64, error) {
	b, err := units.RAMInBytes(c.MaxSize)
	if err != nil {
		return 0, fmt.Errorf("cache: parsing max_size %q: %w", c.MaxSize, err)
	}

	return b, nil
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	size, err := c.MaxSizeBytes()
	if err != nil {
		return err
	}

	if size <= 0 {
		return fmt.Errorf("cache: max_size must be greater than zero")
	}

	return nil
}
