package cache

import (
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		expectError bool
	}{
		{
			name:   "absent block behaves like default config",
			config: *DefaultConfig(),
		},
		{
			name:   "enabled false with nothing else set",
			config: Config{Enabled: false},
		},
		{
			name:        "enabled true missing bind_address",
			config:      Config{Enabled: true, MaxSize: "50GB"},
			expectError: true,
		},
		{
			name:        "enabled true with bind_address but missing max_size",
			config:      Config{Enabled: true, BindAddress: "192.168.128.1:5050"},
			expectError: true,
		},
		{
			name:        "enabled true with bind_address missing port",
			config:      Config{Enabled: true, BindAddress: "192.168.128.1", MaxSize: "50GB"},
			expectError: true,
		},
		{
			name:   "enabled true with 50GB max_size",
			config: Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "50GB"},
		},
		{
			name:   "enabled true with 50GiB max_size",
			config: Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "50GiB"},
		},
		{
			name:   "enabled true with 50G max_size",
			config: Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "50G"},
		},
		{
			name:   "enabled true with bare byte count max_size",
			config: Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "53687091200"},
		},
		{
			name:        "enabled true with unrecognized suffix",
			config:      Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "50XB"},
			expectError: true,
		},
		{
			name:        "enabled true with garbage max_size",
			config:      Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "banana"},
			expectError: true,
		},
		{
			name:        "enabled true with zero max_size",
			config:      Config{Enabled: true, BindAddress: "192.168.128.1:5050", MaxSize: "0"},
			expectError: true,
		},
		{
			name:   "enabled false with garbage max_size is never parsed",
			config: Config{Enabled: false, MaxSize: "banana"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.config

			err := validator.New().Struct(&cfg)
			if err == nil {
				err = cfg.Validate()
			}

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestConfig_MaxSizeBytes(t *testing.T) {
	tests := []struct {
		name    string
		maxSize string
		want    int64
	}{
		{"50GB parses as binary", "50GB", 53687091200},
		{"50GiB parses to the identical value as 50GB", "50GiB", 53687091200},
		{"50G parses to the identical value as 50GB", "50G", 53687091200},
		{"bare number is raw bytes", "53687091200", 53687091200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{MaxSize: tt.maxSize}

			got, err := cfg.MaxSizeBytes()
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConfig_MaxSizeBytes_Error(t *testing.T) {
	tests := []struct {
		name    string
		maxSize string
	}{
		{"unrecognized suffix", "50XB"},
		{"garbage", "banana"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{MaxSize: tt.maxSize}

			_, err := cfg.MaxSizeBytes()
			assert.Error(t, err)
		})
	}
}

func TestConfig_DefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.Enabled)
	assert.Equal(t, DefaultDataDir, cfg.DataDir)
}
