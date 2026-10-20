package service

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Per-bucket storage quotas.
//
// A quota caps the total size of a bucket. It is enforced in the CDN, in front of
// every PutObject, rather than in MinIO, so it works the same on any storage
// backend and can be managed through the API like tokens are.
//
// Usage is not tracked from scratch: it is counted by listing the bucket, cached
// in Redis for usageTTL, bumped on every upload through this API and dropped on
// delete. Anything that changes the bucket behind the CDN's back (mc, the MinIO
// console) is therefore picked up at the next recount, within usageTTL.
//
// Two honest limits. Concurrent uploads can overshoot by what is in flight, since
// check and write are not one atomic step. And overwriting an existing key counts
// the new size on top of the old until the next recount.

const (
	quotaPrefix = "cdn:quota:"
	usagePrefix = "cdn:usage:"
	usageTTL    = 5 * time.Minute
)

// ErrQuotaExceeded is returned when an upload would push a bucket past its quota.
var ErrQuotaExceeded = errors.New("bucket storage quota exceeded")

// QuotaStore keeps quotas and cached usage.
type QuotaStore struct {
	backend tokenBackend
}

func NewQuotaStore(b tokenBackend) *QuotaStore { return &QuotaStore{backend: b} }

// ParseSize reads sizes such as "100M", "500MB", "5GB", "1.5G" or a plain byte
// count. Units are binary (1M = 1024*1024), matching the rest of this service.
func ParseSize(raw string) (int64, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
	s = strings.TrimSuffix(s, "B")
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = 1<<10, s[:n-1]
		case 'M':
			mult, s = 1<<20, s[:n-1]
		case 'G':
			mult, s = 1<<30, s[:n-1]
		case 'T':
			mult, s = 1<<40, s[:n-1]
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q: use a positive number with an optional K/M/G/T suffix, e.g. 500MB", raw)
	}
	return int64(v * float64(mult)), nil
}

// FormatSize renders bytes for humans.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func (q *QuotaStore) SetLimit(bucket string, bytes int64) error {
	return q.backend.Set(quotaPrefix+bucket, strconv.FormatInt(bytes, 10), 0)
}

func (q *QuotaStore) ClearLimit(bucket string) error {
	return q.backend.Del(quotaPrefix+bucket, usagePrefix+bucket)
}

// Limit returns the bucket's quota in bytes; ok is false when none is set.
func (q *QuotaStore) Limit(bucket string) (int64, bool, error) {
	raw, found, err := q.backend.Get(quotaPrefix + bucket)
	if err != nil || !found {
		return 0, false, err
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil, err
}

// Limits returns every configured quota, keyed by bucket.
func (q *QuotaStore) Limits() (map[string]int64, error) {
	keys, err := q.backend.Keys(quotaPrefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(keys))
	for _, k := range keys {
		name := strings.TrimPrefix(k, quotaPrefix)
		if n, ok, err := q.Limit(name); err == nil && ok {
			out[name] = n
		}
	}
	return out, nil
}

// Used returns the bucket's size, from cache or by calling count.
func (q *QuotaStore) Used(bucket string, count func() (int64, error), refresh bool) (int64, error) {
	if !refresh {
		if raw, found, err := q.backend.Get(usagePrefix + bucket); err == nil && found {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
				return n, nil
			}
		}
	}
	n, err := count()
	if err != nil {
		return 0, err
	}
	_ = q.backend.Set(usagePrefix+bucket, strconv.FormatInt(n, 10), usageTTL)
	return n, nil
}

// AddUsed records an upload against the cached usage, if there is a cached usage.
func (q *QuotaStore) AddUsed(bucket string, delta int64) {
	_ = q.backend.IncrExisting(usagePrefix+bucket, delta)
}

// InvalidateUsed drops the cached usage so the next check recounts.
func (q *QuotaStore) InvalidateUsed(bucket string) {
	_ = q.backend.Del(usagePrefix + bucket)
}

// Check reports whether incoming more bytes fit in bucket. A bucket with no quota
// is never counted. If the quota or usage cannot be read, the upload is allowed:
// a Redis outage should not turn into an upload outage.
func (q *QuotaStore) Check(bucket string, incoming int64, count func() (int64, error)) error {
	limit, ok, err := q.Limit(bucket)
	if err != nil || !ok {
		return nil
	}
	used, err := q.Used(bucket, count, false)
	if err != nil {
		return nil
	}
	if used+incoming > limit {
		return fmt.Errorf("%w: %s used of %s, upload of %s does not fit",
			ErrQuotaExceeded, FormatSize(used), FormatSize(limit), FormatSize(incoming))
	}
	return nil
}

var quotas *QuotaStore

// SetQuotaStore installs the process-wide store; nil disables quotas.
func SetQuotaStore(s *QuotaStore) { quotas = s }

// Quotas returns the installed store, or nil when quotas are disabled.
func Quotas() *QuotaStore { return quotas }
