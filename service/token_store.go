package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/mstgnz/cdn/pkg/bucket"
	"github.com/mstgnz/cdn/pkg/config"
)

// Dynamic bucket-scoped tokens, created at runtime through the API.
//
// The file-based tokens in config/tokens.json are read once at boot into
// per-process memory, which cannot work for runtime creation: the API runs as
// several replicas and a token added to one would be unknown to the others. These
// live in the shared backend (Redis) instead, so every replica sees a create or a
// revoke on its next request. The file keeps working untouched; ResolvePrincipal
// consults it first and this store second.
//
// Only a SHA-256 digest of the secret is stored. The secret itself is returned
// once by Create and cannot be recovered afterwards.

const (
	tokenHashPrefix = "cdn:btok:h:"  // digest -> record, the authentication lookup
	tokenIDPrefix   = "cdn:btok:id:" // id -> record, used by list and revoke
)

// TokenInfo is the API-facing description of a token. It carries no secret and
// no digest.
type TokenInfo struct {
	ID        string `json:"id"`
	Bucket    string `json:"bucket"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// tokenRecord is what the backend stores: the public fields plus the digest used
// to authenticate. It is never returned to API clients.
type tokenRecord struct {
	TokenInfo
	Hash string `json:"hash"`
}

// tokenBackend is the small key/value surface the store needs.
type tokenBackend interface {
	Get(key string) (string, bool, error)
	Set(key, value string, ttl time.Duration) error
	Del(keys ...string) error
	Keys(prefix string) ([]string, error)
	// IncrExisting adds delta to an integer value, and does nothing when the key
	// is absent (so an expired cache entry is recounted, not resurrected as delta).
	IncrExisting(key string, delta int64) error
}

// TokenStore creates, authenticates and revokes dynamic bucket tokens.
type TokenStore struct {
	backend tokenBackend
}

// NewTokenStore wraps a backend.
func NewTokenStore(b tokenBackend) *TokenStore { return &TokenStore{backend: b} }

func digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Create mints a token for bucketName and returns the secret exactly once.
// expiresAt may be the zero time for a token that never expires.
func (s *TokenStore) Create(bucketName, label string, expiresAt time.Time) (string, TokenInfo, error) {
	if err := bucket.Validate(bucketName); err != nil {
		return "", TokenInfo{}, err
	}
	if !expiresAt.IsZero() && !expiresAt.After(time.Now()) {
		return "", TokenInfo{}, errors.New("expires_at must be in the future")
	}

	secret, err := randomHex(32)
	if err != nil {
		return "", TokenInfo{}, fmt.Errorf("generate token: %w", err)
	}
	id, err := randomHex(6)
	if err != nil {
		return "", TokenInfo{}, fmt.Errorf("generate token id: %w", err)
	}

	rec := tokenRecord{
		TokenInfo: TokenInfo{
			ID:        id,
			Bucket:    bucketName,
			Label:     label,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		},
		Hash: digest(secret),
	}
	var ttl time.Duration
	if !expiresAt.IsZero() {
		rec.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
		ttl = time.Until(expiresAt)
	}

	raw, err := json.Marshal(rec)
	if err != nil {
		return "", TokenInfo{}, err
	}
	if err := s.backend.Set(tokenHashPrefix+rec.Hash, string(raw), ttl); err != nil {
		return "", TokenInfo{}, err
	}
	if err := s.backend.Set(tokenIDPrefix+rec.ID, string(raw), ttl); err != nil {
		_ = s.backend.Del(tokenHashPrefix + rec.Hash)
		return "", TokenInfo{}, err
	}
	return secret, rec.TokenInfo, nil
}

func decode(raw string) (tokenRecord, error) {
	var rec tokenRecord
	err := json.Unmarshal([]byte(raw), &rec)
	return rec, err
}

// Matches reports whether secret is a live token for bucketName.
func (s *TokenStore) Matches(bucketName, secret string) bool {
	return s.MatchesAt(bucketName, secret, time.Now())
}

// MatchesAt is Matches with an explicit clock, for testing expiry.
//
// The lookup key is a SHA-256 digest of a 256-bit random secret, so finding the
// record is not a timing oracle for the secret. The bucket is still compared, so
// a valid token cannot be replayed against another bucket.
func (s *TokenStore) MatchesAt(bucketName, secret string, now time.Time) bool {
	raw, found, err := s.backend.Get(tokenHashPrefix + digest(secret))
	if err != nil || !found {
		return false
	}
	rec, err := decode(raw)
	if err != nil || rec.Bucket != bucketName {
		return false
	}
	if rec.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, rec.ExpiresAt)
		if err != nil || !now.Before(exp) {
			return false
		}
	}
	return true
}

// List returns token records, optionally filtered to one bucket. Secrets and
// digests are never part of the result's JSON form.
func (s *TokenStore) List(bucketName string) ([]TokenInfo, error) {
	keys, err := s.backend.Keys(tokenIDPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]TokenInfo, 0, len(keys))
	for _, k := range keys {
		raw, found, err := s.backend.Get(k)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		rec, err := decode(raw)
		if err != nil {
			continue
		}
		if bucketName != "" && rec.Bucket != bucketName {
			continue
		}
		out = append(out, rec.TokenInfo)
	}
	return out, nil
}

// Revoke deletes the token with the given id. It reports whether one existed.
func (s *TokenStore) Revoke(id string) (bool, error) {
	raw, found, err := s.backend.Get(tokenIDPrefix + id)
	if err != nil || !found {
		return false, err
	}
	rec, err := decode(raw)
	if err != nil {
		return false, err
	}
	if err := s.backend.Del(tokenHashPrefix+rec.Hash, tokenIDPrefix+id); err != nil {
		return false, err
	}
	return true, nil
}

// --- Redis backend ---------------------------------------------------------

type redisTokenBackend struct {
	client *redis.Client
}

// NewRedisTokenBackend connects to the same Redis the cache uses (REDIS_URL).
// Like the cache, the client is returned even if Redis is not reachable yet:
// go-redis connects lazily and reconnects on its own.
func NewRedisTokenBackend() (tokenBackend, error) {
	opt, err := redis.ParseURL(config.GetEnvOrDefault("REDIS_URL", "redis://cdn-redis:6379"))
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	return &redisTokenBackend{client: redis.NewClient(opt)}, nil
}

func (r *redisTokenBackend) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (r *redisTokenBackend) Get(key string) (string, bool, error) {
	ctx, cancel := r.ctx()
	defer cancel()
	v, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (r *redisTokenBackend) Set(key, value string, ttl time.Duration) error {
	ctx, cancel := r.ctx()
	defer cancel()
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *redisTokenBackend) Del(keys ...string) error {
	ctx, cancel := r.ctx()
	defer cancel()
	return r.client.Del(ctx, keys...).Err()
}

func (r *redisTokenBackend) IncrExisting(key string, delta int64) error {
	ctx, cancel := r.ctx()
	defer cancel()
	// KEEPTTL is not needed: INCRBY preserves the key's existing TTL.
	return incrExisting.Run(ctx, r.client, []string{key}, delta).Err()
}

var incrExisting = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return redis.call('INCRBY', KEYS[1], ARGV[1])
end
return 0`)

func (r *redisTokenBackend) Keys(prefix string) ([]string, error) {
	ctx, cancel := r.ctx()
	defer cancel()
	var out []string
	iter := r.client.Scan(ctx, 0, prefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		out = append(out, iter.Val())
	}
	return out, iter.Err()
}

// --- Wiring ----------------------------------------------------------------

var dynamicTokens *TokenStore

// SetTokenStore installs the store ResolvePrincipal consults after the token file.
// Called once at boot; nil disables dynamic tokens.
func SetTokenStore(s *TokenStore) { dynamicTokens = s }

// DynamicTokens returns the installed store, or nil when disabled.
func DynamicTokens() *TokenStore { return dynamicTokens }

// NewMemoryTokenBackend returns an in-process backend. It exists for tests of
// packages that build a QuotaStore or TokenStore without Redis.
func NewMemoryTokenBackend() tokenBackend { return &memoryBackend{data: map[string]string{}} }

type memoryBackend struct {
	mu   sync.Mutex
	data map[string]string
}

func (m *memoryBackend) Get(k string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[k]
	return v, ok, nil
}
func (m *memoryBackend) Set(k, v string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = v
	return nil
}
func (m *memoryBackend) Del(keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.data, k)
	}
	return nil
}
func (m *memoryBackend) IncrExisting(k string, d int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.data[k]; ok {
		n, _ := strconv.ParseInt(cur, 10, 64)
		m.data[k] = strconv.FormatInt(n+d, 10)
	}
	return nil
}
func (m *memoryBackend) Keys(prefix string) ([]string, error) {
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
