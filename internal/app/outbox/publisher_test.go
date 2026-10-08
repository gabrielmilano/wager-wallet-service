package outbox

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	max := 5 * time.Minute
	tests := map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second, 10: max, 50: max}
	for attempts, want := range tests {
		if got := Backoff(attempts, max); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}
