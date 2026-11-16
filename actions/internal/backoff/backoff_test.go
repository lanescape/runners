package backoff

import (
	"net/http"
	"testing"
	"time"
)

func TestWait(t *testing.T) {
	p := Policy{Attempts: 8, First: 2 * time.Second, Max: time.Minute}
	for n, want := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 6: time.Minute, 7: time.Minute} {
		if got := p.Wait(n, 0); got != want {
			t.Errorf("wait %d = %s, want %s", n, got, want)
		}
	}
	if got := p.Wait(1, 5*time.Second); got != 5*time.Second {
		t.Errorf("wait with Retry-After = %s", got)
	}
	if got := p.Wait(1, time.Hour); got != time.Minute {
		t.Errorf("wait with a long Retry-After = %s", got)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{"": 0, "5": 5 * time.Second, "x": 0, "-1": 0,
		now.Add(30 * time.Second).Format(http.TimeFormat): 30 * time.Second} {
		h := http.Header{}
		h.Set("Retry-After", v)
		if got := RetryAfter(h, now); got != want {
			t.Errorf("Retry-After %q = %s, want %s", v, got, want)
		}
	}
}
