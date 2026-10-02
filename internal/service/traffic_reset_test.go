package service

import (
	"testing"
	"time"
)

func tm(loc *time.Location, y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 10, 0, 0, 0, loc)
}

func ptr(t time.Time) *time.Time {
	return &t
}

func TestShouldResetNodeTrafficNow_FixedDayFallbackToMonthEnd(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := tm(loc, 2026, time.April, 30) // April has 30 days.
	lastReset := tm(loc, 2026, time.March, 31)

	if !shouldResetNodeTrafficNow(TrafficResetModeFixedDay, 31, 30, ptr(lastReset), nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected fixed day fallback (31->30) to reset on month end")
	}
}

func TestNormalizeTrafficResetMode_BackwardCompatibilityByResetDay(t *testing.T) {
	if got := NormalizeTrafficResetMode("", 10); got != TrafficResetModeFixedDay {
		t.Fatalf("expected empty mode + reset_day>0 to map to fixed_day, got=%s", got)
	}
	if got := NormalizeTrafficResetMode("", 0); got != TrafficResetModeOff {
		t.Fatalf("expected empty mode + reset_day<=0 to map to off, got=%s", got)
	}
}

func TestNormalizeTrafficResetMode_FixedDayWithZeroDayFallsBackOff(t *testing.T) {
	if got := NormalizeTrafficResetMode(TrafficResetModeFixedDay, 0); got != TrafficResetModeOff {
		t.Fatalf("expected fixed_day + reset_day<=0 to map to off, got=%s", got)
	}
	if got := NormalizeTrafficResetMode("fixed", -1); got != TrafficResetModeOff {
		t.Fatalf("expected fixed alias + reset_day<=0 to map to off, got=%s", got)
	}
}

func TestShouldResetNodeTrafficNow_FixedDayNotYetDue(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := tm(loc, 2026, time.April, 15)
	lastReset := tm(loc, 2026, time.March, 20)

	if shouldResetNodeTrafficNow(TrafficResetModeFixedDay, 20, 30, ptr(lastReset), nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected fixed day mode not to reset before target day")
	}
}

func TestResolveTrafficResetAtOnRuleChange_FixedDayFutureTargetAllowsCurrentMonthReset(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)

	oldLocal := time.Local
	time.Local = loc
	defer func() { time.Local = oldLocal }()

	changedAt := time.Date(2026, time.April, 1, 10, 0, 0, 0, loc)
	lastReset := ResolveTrafficResetAtOnRuleChange(TrafficResetModeFixedDay, 2, changedAt)
	if lastReset == nil {
		t.Fatalf("expected fixed day rule change to initialize last reset reference")
	}

	now := time.Date(2026, time.April, 2, 0, 1, 0, 0, loc)
	if !shouldResetNodeTrafficNow(TrafficResetModeFixedDay, 2, 30, lastReset, nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected Apr 1 rule change for day 2 to still reset on Apr 2")
	}
}

func TestShouldResetNodeTrafficNow_FixedDaySameMonthBeforeTargetDoesNotCountAsReset(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	oldLocal := time.Local
	time.Local = loc
	defer func() { time.Local = oldLocal }()

	now := time.Date(2026, time.April, 2, 0, 1, 0, 0, loc)
	lastReset := time.Date(2026, time.April, 1, 10, 0, 0, 0, loc)

	if !shouldResetNodeTrafficNow(TrafficResetModeFixedDay, 2, 30, ptr(lastReset), nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected same-month timestamp before target day not to block fixed day reset")
	}
}

func TestResolveTrafficResetAtOnRuleChange_FixedDayPastTargetDoesNotImmediateCatchUp(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)

	oldLocal := time.Local
	time.Local = loc
	defer func() { time.Local = oldLocal }()

	changedAt := time.Date(2026, time.April, 3, 10, 0, 0, 0, loc)
	lastReset := ResolveTrafficResetAtOnRuleChange(TrafficResetModeFixedDay, 2, changedAt)
	if lastReset == nil {
		t.Fatalf("expected fixed day rule change to initialize last reset reference")
	}

	now := time.Date(2026, time.April, 3, 10, 1, 0, 0, loc)
	if shouldResetNodeTrafficNow(TrafficResetModeFixedDay, 2, 30, lastReset, nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected enabling fixed day after target day not to catch up immediately in same month")
	}
}

func TestShouldResetNodeTrafficNow_CalendarMonthCatchUp(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := tm(loc, 2026, time.April, 2)
	lastReset := tm(loc, 2026, time.March, 1)

	if !shouldResetNodeTrafficNow(TrafficResetModeCalendarMonth, 0, 30, ptr(lastReset), nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected calendar month mode to catch up when not reset in current month")
	}
}

func TestShouldResetNodeTrafficNow_IntervalDays(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := tm(loc, 2026, time.April, 10)
	lastReset := tm(loc, 2026, time.April, 1)

	if !shouldResetNodeTrafficNow(TrafficResetModeIntervalDays, 1, 7, ptr(lastReset), nil, tm(loc, 2025, time.January, 1), now) {
		t.Fatalf("expected interval mode to reset when interval elapsed")
	}
}

func TestShouldResetNodeTrafficNow_IntervalDays_WithAnchorDate(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	anchor := tm(loc, 2026, time.January, 29) // bought on Jan 29
	now := tm(loc, 2026, time.February, 28)   // 30 days later in non-leap year

	if !shouldResetNodeTrafficNow(TrafficResetModeIntervalDays, 1, 30, nil, ptr(anchor), tm(loc, 2026, time.January, 1), now) {
		t.Fatalf("expected 30-day interval to reset based on anchor date Jan 29")
	}
}

func TestShouldResetNodeTrafficNow_IntervalDays_DoesNotDriftWhenLate(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	anchor := tm(loc, 2026, time.January, 29)
	lastResetLate := tm(loc, 2026, time.March, 3) // processed late for cycle 1
	now := tm(loc, 2026, time.March, 30)          // still cycle 2 boundary by anchor

	if !shouldResetNodeTrafficNow(TrafficResetModeIntervalDays, 1, 30, ptr(lastResetLate), ptr(anchor), tm(loc, 2026, time.January, 1), now) {
		t.Fatalf("expected anchored interval cycle not to drift when last reset was late")
	}
}

func TestResolveTrafficResetAnchor_UsesLocalDayFromCreatedAt(t *testing.T) {
	oldLocal := time.Local
	loc := time.FixedZone("UTC+8", 8*3600)
	time.Local = loc
	defer func() { time.Local = oldLocal }()

	createdAtUTC := time.Date(2026, time.January, 28, 16, 30, 0, 0, time.UTC) // local = 2026-01-29 00:30
	got := ResolveTrafficResetAnchor(nil, createdAtUTC, time.Time{})
	want := time.Date(2026, time.January, 29, 0, 0, 0, 0, loc)

	if !got.Equal(want) {
		t.Fatalf("expected anchor=%s, got=%s", want.Format(time.RFC3339), got.Format(time.RFC3339))
	}
}

func TestIntervalCycleIndex_DstSafeDayCount(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("timezone America/New_York not available")
	}

	oldLocal := time.Local
	time.Local = loc
	defer func() { time.Local = oldLocal }()

	anchor := time.Date(2026, time.March, 7, 0, 0, 0, 0, loc)
	twoDaysLater := time.Date(2026, time.March, 9, 0, 0, 0, 0, loc)

	if got := localDateDiffDays(anchor, twoDaysLater); got != 2 {
		t.Fatalf("expected local day diff=2 across DST, got=%d", got)
	}
	if got := intervalCycleIndex(anchor, twoDaysLater, 1); got != 2 {
		t.Fatalf("expected interval cycle index=2 across DST, got=%d", got)
	}
}

func TestDurationUntilNextMinuteBoundary(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)

	now := time.Date(2026, time.April, 2, 15, 12, 56, 0, loc)
	if got := durationUntilNextMinuteBoundary(now); got != 4*time.Second {
		t.Fatalf("expected 4s until next minute boundary, got=%s", got)
	}
}

func TestDurationUntilNextMinuteBoundary_OnExactMinuteWaitsOneMinute(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)

	now := time.Date(2026, time.April, 2, 15, 12, 0, 0, loc)
	if got := durationUntilNextMinuteBoundary(now); got != time.Minute {
		t.Fatalf("expected exact minute boundary to wait one full minute, got=%s", got)
	}
}
