package agent

import (
	"math"
	"testing"
	"time"
)

func TestReconnectBackoffIsBoundedBeforeTheShiftCanOverflow(t *testing.T) {
	const maxBackoff = 60 * time.Second

	tests := []struct {
		name  string
		count int
		want  time.Duration
	}{
		{name: "first attempt", count: 1, want: time.Second},
		{name: "second attempt", count: 2, want: 2 * time.Second},
		{name: "sixth attempt", count: 6, want: 32 * time.Second},
		{name: "capped attempt", count: 7, want: maxBackoff},
		{name: "old panic boundary", count: 35, want: maxBackoff},
		{name: "integer max", count: math.MaxInt, want: maxBackoff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconnectBackoff(tt.count, maxBackoff); got != tt.want {
				t.Fatalf("reconnectBackoff(%d) = %v, want %v", tt.count, got, tt.want)
			}
		})
	}
}

func TestReconnectBackoffHandlesInvalidInputs(t *testing.T) {
	if got := reconnectBackoff(0, time.Minute); got != time.Second {
		t.Fatalf("zero count should use the first backoff, got %v", got)
	}
	if got := reconnectBackoff(-10, time.Minute); got != time.Second {
		t.Fatalf("negative count should use the first backoff, got %v", got)
	}
	if got := reconnectBackoff(1, 0); got != 0 {
		t.Fatalf("zero max backoff should disable the delay, got %v", got)
	}
	if got := reconnectBackoff(1, 500*time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("small max backoff should be honored, got %v", got)
	}
}
