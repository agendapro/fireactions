package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewConfig(t *testing.T) {
	config, err := NewConfig("testdata/config1.yaml")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	assert.Equal(t, "testdata/config1.yaml", config.path)
}

func TestNewConfig_Cache(t *testing.T) {
	config, err := NewConfig("testdata/config2.yaml")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	assert.True(t, config.Cache.Enabled)
	assert.Equal(t, "192.168.128.1:5050", config.Cache.BindAddress)
	assert.Equal(t, "/var/lib/fireactions/cache", config.Cache.DataDir)

	maxSizeBytes, err := config.Cache.MaxSizeBytes()
	assert.NoError(t, err)
	assert.Equal(t, int64(53687091200), maxSizeBytes)
}
