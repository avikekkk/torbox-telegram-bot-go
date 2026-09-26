// Package torbox talks to the TorBox API (https://api.torbox.app/v1): adding
// torrents, NZBs and web downloads, listing them, and requesting CDN links.
//
// Every request goes through the same limits the TorBox docs set per API key,
// with headroom, so the bot backs off before TorBox has to make it:
// https://support.torbox.app/en/articles/13726368-api-rate-limits
package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the TorBox API root.
const DefaultBaseURL = "https://api.torbox.app/v1"

const userAgent = "TorBot/1.0"

// Download kinds. They double as the channel job kind stored in SQLite.
const (
	KindTorrent = "torrent"
	KindUsenet  = "usenet"
	KindWebDL   = "webdl"
)

// Kinds lists the three download libraries.
var Kinds = []string{KindTorrent, KindUsenet, KindWebDL}

// retryAttempts bounds how often one request is sent before giving up.
const retryAttempts = 4

// Error is a TorBox failure whose message is safe to show users. Status is the
// HTTP status when there was one, 0 for network failures.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func errorf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// StatusOf returns the HTTP status behind err, or 0.
func StatusOf(err error) int {
	var tbErr *Error
	if errors.As(err, &tbErr) {
		return tbErr.Status
	}
	return 0
}

// Client is a TorBox API client for one API key.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client

	// global paces every request; create additionally paces the three create
	// endpoints, which TorBox limits far harder than the rest.
	global *bucket
	create *bucket
	// inFlight caps concurrent requests.
	inFlight chan struct{}
	quota    *createQuota
	// retryBase is the first retry delay; each later one doubles it.
	retryBase time.Duration
}

// New returns a client for apiKey. An empty baseURL means DefaultBaseURL.
func New(apiKey, baseURL string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:    apiKey,
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      &http.Client{Timeout: timeout},
		global:    newBucket(4, 8),
		create:    newBucket(0.2, 2),
		inFlight:  make(chan struct{}, 6),
		quota:     newCreateQuota(),
		retryBase: 700 * time.Millisecond,
	}
}

// call is one API request. The body is kept as bytes so a retry can resend it.
type call struct {
	method      string
	path        string
	query       url.Values
	body        []byte
	contentType string
	creates     bool
}

// payload is a decoded TorBox response envelope.
type payload map[string]any

func (p payload) data() any { return p["data"] }

// do sends c, retrying transient failures with exponential backoff.
func (c *Client) do(ctx context.Context, req call) (payload, error) {
	var lastErr error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		result, err := c.once(ctx, req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryable(err) || attempt == retryAttempts-1 {
			return nil, err
		}

		delay := c.retryBase<<attempt + time.Duration(rand.Float64()*float64(300*time.Millisecond))
		if StatusOf(err) == http.StatusTooManyRequests {
			delay = max(delay, 2*time.Second)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// retryable reports whether a failure may pass on its own. Auth and validation
// errors never do, and a cancelled context is final.
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch StatusOf(err) {
	case 0, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (c *Client) once(ctx context.Context, req call) (payload, error) {
	if err := c.global.wait(ctx); err != nil {
		return nil, err
	}
	if req.creates {
		if err := c.create.wait(ctx); err != nil {
			return nil, err
		}
	}
	select {
	case c.inFlight <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.inFlight }()

	endpoint := c.baseURL + req.path
	if len(req.query) > 0 {
		endpoint += "?" + req.query.Encode()
	}
	var body io.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, endpoint, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("User-Agent", userAgent)
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errorf(0, "Network error talking to TorBox: %v", withoutURL(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, errorf(0, "Network error talking to TorBox: %v", withoutURL(err))
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		decoded = map[string]any{"raw": string(raw)}
	}
	result, ok := decoded.(map[string]any)
	if !ok {
		result = map[string]any{"data": decoded}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errorf(http.StatusTooManyRequests, "TorBox rate limited (429). Retry after ~%.0fs.",
			retryAfter(resp.Header.Get("Retry-After")).Seconds())
	}
	if resp.StatusCode >= 400 {
		return nil, errorf(resp.StatusCode, "TorBox HTTP %d: %s", resp.StatusCode,
			errorDetail(result, strings.TrimSpace(string(raw))))
	}
	if success, ok := result["success"].(bool); ok && !success {
		// Some rate limits arrive as a 200 with success=false.
		status := 0
		code := strings.ToUpper(fmt.Sprint(result["error"]))
		if strings.Contains(code, "RATE") || strings.Contains(code, "LIMIT") {
			status = http.StatusTooManyRequests
		}
		return nil, errorf(status, "%s", errorDetail(result, "TorBox request failed"))
	}
	return result, nil
}

// withoutURL drops the request URL from a transport error. requestdl carries
// the API key as a query parameter, so the URL must never reach a log line or
// a chat.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func retryAfter(header string) time.Duration {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil {
		return 3 * time.Second
	}
	return time.Duration(max(seconds, 0.5) * float64(time.Second))
}

// errorDetail normalises TorBox's several error shapes into one short line.
func errorDetail(result map[string]any, fallback string) string {
	detail := result["detail"]
	if detail == nil {
		detail = result["error"]
	}
	if detail == nil {
		detail = result["message"]
	}

	var text string
	switch value := detail.(type) {
	case nil:
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if object, ok := item.(map[string]any); ok {
				parts = append(parts, firstString(object, "msg", "message", "detail"))
				continue
			}
			parts = append(parts, fmt.Sprint(item))
		}
		text = strings.Join(parts, "; ")
	case map[string]any:
		text = firstString(value, "message", "msg", "detail")
	default:
		text = fmt.Sprint(value)
	}

	if text == "" {
		text = fallback
	}
	if text == "" {
		text = "error"
	}
	return clip(text, 500)
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key]; ok && value != nil && fmt.Sprint(value) != "" {
			return fmt.Sprint(value)
		}
	}
	return fmt.Sprint(object)
}

func clip(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
