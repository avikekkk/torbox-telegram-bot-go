// Package nzbhydra searches NZBHydra2 and hands results to the downloader
// configured in Hydra, which for this bot is TorBox.
//
// Both go through Hydra's internal API. Hydra's login is HTTP basic auth,
// embedded in the base URL (https://user:pass@host); Go sends it with every
// request. Hydra 9+ also wants its HYDRA-XSRF-TOKEN cookie echoed back as the
// X-XSRF-TOKEN header on POST/PUT, fetched from the site root on first use.
package nzbhydra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const xsrfCookie = "HYDRA-XSRF-TOKEN"

// resultIDPattern matches Hydra's search result IDs, signed 64-bit integers
// such as "-7574822274760567514". Anything else is refused before it reaches
// Hydra.
var resultIDPattern = regexp.MustCompile(`^-?\d{1,20}$`)

var tagPattern = regexp.MustCompile(`<[^>]*>`)

// ValidResultID reports whether value can be a Hydra search result ID.
func ValidResultID(value string) bool { return resultIDPattern.MatchString(value) }

// Error is a Hydra failure whose message is safe to show users: it never
// carries the URL, which holds the Hydra login.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errorf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

// Result is one search hit.
type Result struct {
	ID    string
	Title string
	Size  int64
	// Published is zero when Hydra did not say.
	Published time.Time
	Indexer   string
}

// Client talks to one Hydra instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client for baseURL, which config.ParseNZBHydraURL normalised.
func New(baseURL string, timeout time.Duration) *Client {
	jar, _ := cookiejar.New(nil) // New never fails with nil options.
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout, Jar: jar},
	}
}

// Search runs a query across every indexer Hydra has.
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	var decoded struct {
		SearchResults []struct {
			SearchResultID json.RawMessage `json:"searchResultId"`
			Title          string          `json:"title"`
			Size           json.RawMessage `json:"size"`
			Epoch          json.RawMessage `json:"epoch"`
			Indexer        string          `json:"indexer"`
		} `json:"searchResults"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/internalapi/search",
		map[string]string{"query": query, "category": "All", "mode": "search"}, &decoded)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(decoded.SearchResults))
	for _, item := range decoded.SearchResults {
		id := rawText(item.SearchResultID)
		if !ValidResultID(id) {
			continue
		}
		result := Result{
			ID:      id,
			Title:   strings.TrimSpace(tagPattern.ReplaceAllString(item.Title, "")),
			Size:    rawInt(item.Size),
			Indexer: item.Indexer,
		}
		if result.Title == "" {
			result.Title = "Untitled"
		}
		if epoch := rawInt(item.Epoch); epoch > 0 {
			result.Published = time.Unix(epoch, 0)
		}
		results = append(results, result)
	}
	return results, nil
}

// Add hands a search result to a downloader configured in Hydra.
func (c *Client) Add(ctx context.Context, resultID, downloaderName string) error {
	if !ValidResultID(resultID) {
		return errorf("Invalid NZB ID")
	}
	var decoded struct {
		Successful bool              `json:"successful"`
		Message    string            `json:"message"`
		MissedIDs  []json.RawMessage `json:"missedIds"`
	}
	err := c.doJSON(ctx, http.MethodPut, "/internalapi/downloader/addNzbs", map[string]any{
		"downloaderName": downloaderName,
		"searchResults":  []map[string]string{{"searchResultId": resultID}},
		"category":       "",
	}, &decoded)
	if err != nil {
		return err
	}

	missed := false
	for _, id := range decoded.MissedIDs {
		if rawText(id) == resultID {
			missed = true
		}
	}
	if !decoded.Successful || missed {
		detail := ""
		if decoded.Message != "" {
			detail = ": " + decoded.Message
		}
		return errorf("NZBHydra could not add it to %s%s (the result may have expired; search again)",
			downloaderName, detail)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return errorf("NZBHydra URL is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	token, err := c.xsrfToken(ctx)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-XSRF-TOKEN", token)
	}

	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return errorf("NZBHydra returned an unreadable response")
	}
	return nil
}

// send performs a request and maps every failure to a message without the URL.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Timeout() {
			return nil, errorf("NZBHydra timed out")
		}
		return nil, errorf("Could not reach NZBHydra")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		drain(resp)
		return nil, errorf("NZBHydra rejected the credentials (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		drain(resp)
		return nil, errorf("NZBHydra returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// xsrfToken returns Hydra's XSRF cookie, visiting the site root first if the
// jar has none. An empty token means Hydra does not use CSRF protection.
func (c *Client) xsrfToken(ctx context.Context) (string, error) {
	root, err := url.Parse(c.baseURL + "/")
	if err != nil {
		return "", errorf("NZBHydra URL is invalid")
	}
	find := func() string {
		for _, cookie := range c.http.Jar.Cookies(root) {
			if cookie.Name == xsrfCookie {
				return cookie.Value
			}
		}
		return ""
	}
	if token := find(); token != "" {
		return token, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root.String(), nil)
	if err != nil {
		return "", errorf("NZBHydra URL is invalid")
	}
	resp, err := c.send(req)
	if err != nil {
		return "", err
	}
	drain(resp)
	return find(), nil
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// rawText reads a JSON value that may be a string or a number, keeping every
// digit: a result ID decoded through float64 would lose its last few.
func rawText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(string(raw))
}

func rawInt(raw json.RawMessage) int64 {
	text := rawText(raw)
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return int64(f)
	}
	return 0
}
