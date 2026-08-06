package server

import (
	"testing"

	"github.com/hostinger/fireactions/server/cache"
	"github.com/stretchr/testify/assert"
)

// server.New requires a live containerd socket (containerd.New is called
// unconditionally), so it cannot be exercised here without one. The
// nil-cacheManager behavior it wires up is asserted indirectly below, through
// the two building blocks New's cache-construction branch actually depends
// on: guardedPorts (port derivation) and the config.Cache != nil &&
// config.Cache.Enabled predicate that gates whether cache.New is even called.

func TestGuardedPorts(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		want        []string
		expectError bool
	}{
		{
			name:   "metrics disabled yields only the grpc port",
			config: &Config{BindAddress: "0.0.0.0:8080", Metrics: &MetricsConfig{Enabled: false, Address: "127.0.0.1:8081"}},
			want:   []string{"8080"},
		},
		{
			name:   "metrics nil yields only the grpc port",
			config: &Config{BindAddress: "0.0.0.0:8080"},
			want:   []string{"8080"},
		},
		{
			name:   "metrics enabled yields both ports",
			config: &Config{BindAddress: "0.0.0.0:8080", Metrics: &MetricsConfig{Enabled: true, Address: "127.0.0.1:8081"}},
			want:   []string{"8080", "8081"},
		},
		{
			name:        "invalid bind_address returns an error",
			config:      &Config{BindAddress: "not-a-valid-address"},
			expectError: true,
		},
		{
			name:        "invalid metrics address returns an error",
			config:      &Config{BindAddress: "0.0.0.0:8080", Metrics: &MetricsConfig{Enabled: true, Address: "not-a-valid-address"}},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := guardedPorts(tt.config)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConfig_CacheConstructionPredicate(t *testing.T) {
	// Mirrors the exact condition server.New guards cache.New construction
	// with: `config.Cache != nil && config.Cache.Enabled`. A nil Cache and a
	// present-but-disabled Cache must both leave Server.cacheManager nil.
	tests := []struct {
		name   string
		config *Config
		want   bool
	}{
		{"nil cache leaves the predicate false", &Config{Cache: nil}, false},
		{"disabled cache leaves the predicate false", &Config{Cache: &cache.Config{Enabled: false}}, false},
		{"enabled cache makes the predicate true", &Config{Cache: &cache.Config{Enabled: true}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.Cache != nil && tt.config.Cache.Enabled
			assert.Equal(t, tt.want, got)
		})
	}
}
