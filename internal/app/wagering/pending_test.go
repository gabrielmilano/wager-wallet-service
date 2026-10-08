package wagering

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	tests := map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second, 10: MaxRetryDelay, 40: MaxRetryDelay}
	for attempt, want := range tests {
		if got := RetryDelay(attempt); got != want {
			t.Errorf("RetryDelay(%d) = %v, want %v", attempt, got, want)
		}
	}
}
