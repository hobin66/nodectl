package singbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestartDoesNotLetOldWatcherClobberNewProcess(t *testing.T) {
	tmp := t.TempDir()

	pidsPath := filepath.Join(tmp, "pids")
	t.Setenv("FAKE_SINGBOX_PIDS", pidsPath)

	binaryPath := filepath.Join(tmp, "fake-sing-box")
	script := `#!/bin/sh
echo "$$" >> "$FAKE_SINGBOX_PIDS"
while true; do sleep 1; done
`
	if err := os.WriteFile(binaryPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake sing-box: %v", err)
	}

	configPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := NewConfigManagerWithPaths(configPath, filepath.Join(tmp, "protocols.json"), filepath.Join(tmp, "certs"))
	m := NewManagerWithConfig(cfg, NewInstaller(binaryPath))
	m.logPath = filepath.Join(tmp, "singbox.log")
	m.pidPath = filepath.Join(tmp, "singbox.pid")
	m.restartDelay = 100 * time.Millisecond
	t.Cleanup(m.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	firstPID := m.Status().PID
	if firstPID <= 0 {
		t.Fatalf("first pid was not recorded")
	}
	waitForFakeStarts(t, pidsPath, 1)

	if err := m.Restart(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
	secondPID := m.Status().PID
	if secondPID <= 0 || secondPID == firstPID {
		t.Fatalf("restart did not record a new pid: first=%d second=%d", firstPID, secondPID)
	}
	waitForFakeStarts(t, pidsPath, 2)

	time.Sleep(350 * time.Millisecond)

	status := m.Status()
	if !status.Running {
		t.Fatalf("manager was marked stopped after restart; last_error=%q", status.LastError)
	}
	if status.PID != secondPID {
		t.Fatalf("manager pid changed unexpectedly: got %d want %d", status.PID, secondPID)
	}

	data, err := os.ReadFile(pidsPath)
	if err != nil {
		t.Fatalf("read fake pids: %v", err)
	}
	starts := strings.Fields(string(data))
	if len(starts) != 2 {
		t.Fatalf("expected exactly two child starts, got %d: %v", len(starts), starts)
	}
}

func waitForFakeStarts(t *testing.T, pidsPath string, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidsPath)
		if err == nil && len(strings.Fields(string(data))) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	data, _ := os.ReadFile(pidsPath)
	t.Fatalf("timed out waiting for %d fake sing-box starts, got %d: %v",
		want, len(strings.Fields(string(data))), strings.Fields(string(data)))
}
