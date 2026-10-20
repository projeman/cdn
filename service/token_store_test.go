package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memBackend is an in-memory tokenBackend so the store logic runs without Redis.
type memBackend struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemBackend() *memBackend { return &memBackend{data: map[string]string{}} }

func (m *memBackend) Get(key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	return v, ok, nil
}

func (m *memBackend) Set(key, value string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *memBackend) Del(keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.data, k)
	}
	return nil
}

func (m *memBackend) IncrExisting(key string, delta int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.data[key]
	if !ok {
		return nil
	}
	n, _ := strconv.ParseInt(cur, 10, 64)
	m.data[key] = strconv.FormatInt(n+delta, 10)
	return nil
}

func (m *memBackend) Keys(prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

func TestTokenStoreCreateAndMatch(t *testing.T) {
	s := NewTokenStore(newMemBackend())

	secret, rec, err := s.Create("test", "panel", time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(secret) < 32 {
		t.Fatalf("secret too short: %d", len(secret))
	}
	if rec.ID == "" || rec.Bucket != "test" || rec.Label != "panel" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if !s.Matches("test", secret) {
		t.Fatal("fresh token must match its own bucket")
	}
	if s.Matches("other", secret) {
		t.Fatal("token must not match a different bucket")
	}
	if s.Matches("test", secret+"x") {
		t.Fatal("wrong secret must not match")
	}
}

func TestTokenStoreMultipleTokensPerBucket(t *testing.T) {
	s := NewTokenStore(newMemBackend())
	a, _, _ := s.Create("test", "a", time.Time{})
	b, _, _ := s.Create("test", "b", time.Time{})
	if a == b {
		t.Fatal("two tokens must differ")
	}
	if !s.Matches("test", a) || !s.Matches("test", b) {
		t.Fatal("both tokens must be valid at once")
	}
}

func TestTokenStoreExpiry(t *testing.T) {
	s := NewTokenStore(newMemBackend())
	secret, _, _ := s.Create("test", "", time.Now().Add(time.Hour))
	if !s.MatchesAt("test", secret, time.Now()) {
		t.Fatal("unexpired token must match")
	}
	if s.MatchesAt("test", secret, time.Now().Add(2*time.Hour)) {
		t.Fatal("expired token must not match")
	}
}

func TestTokenStoreRevoke(t *testing.T) {
	s := NewTokenStore(newMemBackend())
	a, ra, _ := s.Create("test", "a", time.Time{})
	b, _, _ := s.Create("test", "b", time.Time{})

	ok, err := s.Revoke(ra.ID)
	if err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	if s.Matches("test", a) {
		t.Fatal("revoked token must stop working")
	}
	if !s.Matches("test", b) {
		t.Fatal("revoking one token must not affect the other")
	}
	if ok, _ := s.Revoke(ra.ID); ok {
		t.Fatal("revoking twice must report not found")
	}
}

func TestTokenStoreListNeverExposesSecret(t *testing.T) {
	s := NewTokenStore(newMemBackend())
	secret, _, _ := s.Create("test", "a", time.Time{})
	_, _, _ = s.Create("other", "b", time.Time{})

	all, err := s.List("")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v len=%d", err, len(all))
	}
	only, _ := s.List("test")
	if len(only) != 1 || only[0].Bucket != "test" {
		t.Fatalf("list filtered: %+v", only)
	}
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), digest(secret)) {
		t.Fatal("list output must carry neither the secret nor its digest")
	}
}

func TestTokenStoreRevokeBucket(t *testing.T) {
	s := NewTokenStore(newMemBackend())
	a, _, _ := s.Create("gone", "a", time.Time{})
	b, _, _ := s.Create("gone", "b", time.Time{})
	keep, _, _ := s.Create("kept", "c", time.Time{})

	n, err := s.RevokeBucket("gone")
	if err != nil || n != 2 {
		t.Fatalf("RevokeBucket = %d, %v; want 2, nil", n, err)
	}
	if s.Matches("gone", a) || s.Matches("gone", b) {
		t.Fatal("tokens of a deleted bucket must stop working")
	}
	if !s.Matches("kept", keep) {
		t.Fatal("other buckets' tokens must be untouched")
	}
	if n, _ := s.RevokeBucket("gone"); n != 0 {
		t.Fatalf("second RevokeBucket = %d, want 0", n)
	}
}
