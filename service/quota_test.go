package service

import (
	"errors"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"100M": 100 << 20, "100MB": 100 << 20, "500mb": 500 << 20,
		"5GB": 5 << 30, "5g": 5 << 30, "1.5G": 3 << 29, "2048": 2048, "1KB": 1024, " 10 MB ": 10 << 20,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-5M", "0", "5XB", "M"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) must fail", bad)
		}
	}
}

func counter(n int64, calls *int) func() (int64, error) {
	return func() (int64, error) { *calls++; return n, nil }
}

func TestQuotaCheck(t *testing.T) {
	q := NewQuotaStore(newMemBackend())
	calls := 0

	// No quota set: always allowed, and the bucket is never listed.
	if err := q.Check("x", 1<<40, counter(0, &calls)); err != nil || calls != 0 {
		t.Fatalf("no quota must allow without counting: err=%v calls=%d", err, calls)
	}

	_ = q.SetLimit("x", 100<<20)
	if err := q.Check("x", 40<<20, counter(50<<20, &calls)); err != nil {
		t.Fatalf("50+40 of 100 must pass: %v", err)
	}
	if err := q.Check("x", 60<<20, counter(50<<20, &calls)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("50+60 of 100 must fail with ErrQuotaExceeded, got %v", err)
	}
	if err := q.Check("x", 50<<20, counter(50<<20, &calls)); err != nil {
		t.Fatalf("exactly at the limit must pass: %v", err)
	}
	if calls != 1 {
		t.Fatalf("usage must be counted once and then served from cache, calls=%d", calls)
	}
}

func TestQuotaUsageTracking(t *testing.T) {
	q := NewQuotaStore(newMemBackend())
	_ = q.SetLimit("x", 100)
	calls := 0
	_ = q.Check("x", 1, counter(10, &calls)) // primes the cache at 10

	q.AddUsed("x", 85)
	if err := q.Check("x", 10, counter(0, &calls)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("10+85+10 > 100 must fail, got %v", err)
	}

	q.InvalidateUsed("x") // a delete happened: recount from storage
	if err := q.Check("x", 10, counter(20, &calls)); err != nil {
		t.Fatalf("after invalidate the recount (20) must be used: %v", err)
	}
}

func TestQuotaLimitCRUD(t *testing.T) {
	q := NewQuotaStore(newMemBackend())
	if _, ok, _ := q.Limit("x"); ok {
		t.Fatal("unset quota must report not set")
	}
	_ = q.SetLimit("x", 500<<20)
	_ = q.SetLimit("y", 5<<30)
	if l, ok, _ := q.Limit("x"); !ok || l != 500<<20 {
		t.Fatalf("limit x = %d %v", l, ok)
	}
	all, _ := q.Limits()
	if len(all) != 2 || all["y"] != 5<<30 {
		t.Fatalf("limits = %v", all)
	}
	_ = q.ClearLimit("x")
	if _, ok, _ := q.Limit("x"); ok {
		t.Fatal("cleared quota must be gone")
	}
}
