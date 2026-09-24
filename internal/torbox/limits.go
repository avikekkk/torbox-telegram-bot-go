package torbox

import (
	"context"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// bucket is a token-bucket rate limiter.
type bucket struct {
	mu       sync.Mutex
	rate     float64 // tokens refilled per second
	capacity float64 // largest burst
	tokens   float64
	updated  time.Time
}

func newBucket(rate, capacity float64) *bucket {
	return &bucket{rate: rate, capacity: capacity, tokens: capacity, updated: time.Now()}
}

// wait blocks until a token is free or ctx ends.
func (b *bucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens = min(b.capacity, b.tokens+now.Sub(b.updated).Seconds()*b.rate)
		b.updated = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()

		// Jitter, so waiters released together do not stampede the next token.
		jitter := time.Duration(rand.Float64() * float64(50*time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(need + jitter):
		}
	}
}

// createsPerHour is TorBox's documented create limit for each kind: 60/hour
// for createusenetdownload, createwebdownload and uncached createtorrent.
const createsPerHour = 60

// createHeadroom stops creating this many short of the documented limit, so
// the bot backs off before TorBox answers with hard 429s.
const createHeadroom = 5

// RateLimitsURL is where TorBox documents its API limits.
const RateLimitsURL = "https://support.torbox.app/en/articles/13726368-api-rate-limits"

// createQuota counts creates per kind over a rolling hour.
type createQuota struct {
	mu     sync.Mutex
	window time.Duration
	stamps map[string][]time.Time
}

func newCreateQuota() *createQuota {
	return &createQuota{window: time.Hour, stamps: map[string][]time.Time{}}
}

// check reports a user-facing error when kind has no creates left this hour.
// It records nothing: a create counts only once TorBox accepts it.
func (q *createQuota) check(kind string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	softMax := createsPerHour - createHeadroom
	if len(q.prune(kind)) < softMax {
		return nil
	}
	return errorf(http.StatusTooManyRequests,
		"TorBox create limit for %s: ~%d/hour per API key (docs). Soft stop at %d/hour to avoid hard rate limits. "+
			"Wait before adding more uncached items. See %s",
		kind, createsPerHour, softMax, RateLimitsURL)
}

// record counts one accepted create.
func (q *createQuota) record(kind string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stamps[kind] = append(q.prune(kind), time.Now())
}

// prune drops creates older than the window. Callers hold the lock.
func (q *createQuota) prune(kind string) []time.Time {
	cutoff := time.Now().Add(-q.window)
	stamps := q.stamps[kind]
	keep := 0
	for keep < len(stamps) && stamps[keep].Before(cutoff) {
		keep++
	}
	q.stamps[kind] = stamps[keep:]
	return q.stamps[kind]
}

// CheckCreate reports whether kind may be created now, without recording one.
// NZBHydra adds reach TorBox through Hydra's own downloader, so the bot checks
// and records them itself to keep them inside the same budget.
func (c *Client) CheckCreate(kind string) error { return c.quota.check(kind) }

// RecordCreate counts a create made on the bot's behalf by someone else.
func (c *Client) RecordCreate(kind string) { c.quota.record(kind) }
