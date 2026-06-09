package singbox

import (
	"encoding/json"
	"testing"
)

func TestGenerateConfigUsesAgentControlledWarnLogLevel(t *testing.T) {
	cm := NewConfigManager()

	data, err := cm.GenerateConfig()
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}

	var cfg struct {
		Log struct {
			Level     string `json:"level"`
			Timestamp bool   `json:"timestamp"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal generated config: %v", err)
	}

	if cfg.Log.Level != "warn" {
		t.Fatalf("log level = %q, want warn", cfg.Log.Level)
	}
	if !cfg.Log.Timestamp {
		t.Fatalf("log timestamp should remain enabled")
	}
}
