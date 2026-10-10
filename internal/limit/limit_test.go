package limit

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(rate.Limit(1), 2)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("burst %d refused", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("ok %v wait %v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("keys must be independent")
	}
	now = now.Add(1100 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("no refill")
	}
}

func TestRefusedCallDoesNotConsume(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(rate.Limit(1), 1)
	l.now = func() time.Time { return now }
	l.Allow("a")
	for i := 0; i < 50; i++ {
		l.Allow("a") // hammering must not push the wait further away
	}
	now = now.Add(1100 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("refused calls consumed tokens")
	}
}

func TestIdleBucketsSwept(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(rate.Limit(1), 1)
	l.now = func() time.Time { return now }
	l.Allow("old")
	now = now.Add(11 * time.Minute)
	l.Allow("new")
	if len(l.m) != 1 {
		t.Fatalf("%d buckets", len(l.m))
	}
}
