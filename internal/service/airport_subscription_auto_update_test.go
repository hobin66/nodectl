package service

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nodectl/internal/database"
	"nodectl/internal/logger"
)

func TestAirportAutoUpdateFailureWaitsForInterval(t *testing.T) {
	var logOutput bytes.Buffer
	oldLog := logger.Log
	logger.Log = slog.New(slog.NewTextHandler(&logOutput, nil))
	t.Cleanup(func() { logger.Log = oldLog })

	start := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	now := start
	interval := 6 * time.Hour
	sub := database.AirportSub{ID: "failed-sub", Name: "机场 A", UpdatedAt: start.Add(-7 * time.Hour)}
	attempts := &airportAutoAttemptState{}
	calls := 0
	syncSub := func(id string) error {
		calls++
		if id != sub.ID {
			t.Fatalf("unexpected subscription ID %q", id)
		}
		return errors.New("subscription source unavailable")
	}
	clock := func() time.Time { return now }
	wait := func(time.Duration) {}

	attempted, success, failed := syncDueAirportSubscriptions([]database.AirportSub{sub}, interval, attempts, clock, syncSub, wait)
	if attempted != 1 || success != 0 || failed != 1 || calls != 1 {
		t.Fatalf("first run: attempted=%d success=%d failed=%d calls=%d", attempted, success, failed, calls)
	}

	logLength := logOutput.Len()
	now = start.Add(time.Minute)
	attempted, success, failed = syncDueAirportSubscriptions([]database.AirportSub{sub}, interval, attempts, clock, syncSub, wait)
	if attempted != 0 || success != 0 || failed != 0 || calls != 1 {
		t.Fatalf("one minute later: attempted=%d success=%d failed=%d calls=%d", attempted, success, failed, calls)
	}
	if laterLogs := logOutput.String()[logLength:]; strings.Contains(laterLogs, "开始自动同步") || strings.Contains(laterLogs, "自动同步完成") {
		t.Fatalf("due=0 must not write sync start/finish logs: %q", laterLogs)
	}

	now = start.Add(interval)
	attempted, success, failed = syncDueAirportSubscriptions([]database.AirportSub{sub}, interval, attempts, clock, syncSub, wait)
	if attempted != 1 || success != 0 || failed != 1 || calls != 2 {
		t.Fatalf("after interval: attempted=%d success=%d failed=%d calls=%d", attempted, success, failed, calls)
	}
	if !sub.UpdatedAt.Equal(start.Add(-7 * time.Hour)) {
		t.Fatal("failed auto update changed the subscription success timestamp")
	}
}

func TestAirportAutoUpdateDueUsesLatestActivity(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	interval := 6 * time.Hour
	if airportAutoUpdateDue(now.Add(-2*time.Hour), now.Add(-7*time.Hour), now, interval) {
		t.Fatal("newer database update must delay the automatic attempt")
	}
	if airportAutoUpdateDue(now.Add(-7*time.Hour), now.Add(-time.Minute), now, interval) {
		t.Fatal("newer failed attempt must delay the automatic retry")
	}
}

func TestAirportAutoAttemptIsThreadSafe(t *testing.T) {
	now := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	sub := database.AirportSub{ID: "one-sub", UpdatedAt: now.Add(-7 * time.Hour)}
	attempts := &airportAutoAttemptState{}
	var started atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if attempts.beginAttempt(sub, now, 6*time.Hour) {
				started.Add(1)
			}
		}()
	}
	group.Wait()
	if got := started.Load(); got != 1 {
		t.Fatalf("concurrent attempts started %d times, want 1", got)
	}
}
