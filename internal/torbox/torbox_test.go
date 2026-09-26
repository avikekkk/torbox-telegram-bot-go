package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient points a client at handler. The limiter buckets are widened so
// tests are not paced like production traffic.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := New("secret-key", server.URL, 5*time.Second)
	client.global = newBucket(1000, 1000)
	client.create = newBucket(1000, 1000)
	client.retryBase = time.Millisecond
	return client
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func TestAddTorrentPostsMagnetThenRequestsLink(t *testing.T) {
	var calls []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/torrents/createtorrent":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				t.Errorf("content type = %q, want multipart", r.Header.Get("Content-Type"))
			}
			if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
				t.Errorf("Authorization = %q", got)
			}
			if got := r.FormValue("magnet"); got != "magnet:?xt=urn:btih:abc" {
				t.Errorf("magnet = %q", got)
			}
			writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"torrent_id": 99, "name": "Film"}})
		case "/api/torrents/requestdl":
			query := r.URL.Query()
			if query.Get("token") != "secret-key" || query.Get("torrent_id") != "99" || query.Get("zip_link") != "true" {
				t.Errorf("requestdl query = %v", query)
			}
			writeJSON(w, 200, map[string]any{"success": true, "data": "https://cdn.example/file"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})

	link, err := client.AddTorrent(context.Background(), "magnet:?xt=urn:btih:abc", "", nil, "")
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if link.ID != 99 || link.Name != "Film" || link.URL != "https://cdn.example/file" || link.Kind != KindTorrent {
		t.Errorf("link = %+v", link)
	}
	// Create must come first: requestdl only knows a download once it exists.
	if len(calls) != 2 || calls[0] != "POST /api/torrents/createtorrent" {
		t.Errorf("calls = %v", calls)
	}
}

func TestAddUncachedHasNoURLButSucceeds(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usenet/createusenetdownload" {
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("no file part: %v", err)
			}
			content, _ := io.ReadAll(file)
			if header.Filename != "a.nzb" || string(content) != "<nzb/>" {
				t.Errorf("file = %q %q", header.Filename, content)
			}
			if r.FormValue("post_processing") != "-1" {
				t.Errorf("post_processing = %q", r.FormValue("post_processing"))
			}
			writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"usenetdownload_id": 7}})
			return
		}
		writeJSON(w, 400, map[string]any{"success": false, "detail": "Download is not ready"})
	})

	link, err := client.AddUsenet(context.Background(), []byte("<nzb/>"), "a.nzb", "a")
	if err != nil {
		t.Fatalf("AddUsenet: %v", err)
	}
	if link.ID != 7 || link.URL != "" || link.Name != "a" {
		t.Errorf("link = %+v", link)
	}
}

func TestSuccessFalseIsAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"success": false, "detail": "No slots"})
	})
	_, err := client.AddTorrent(context.Background(), "magnet:?xt=urn:btih:aa", "", nil, "")
	if err == nil || err.Error() != "No slots" {
		t.Fatalf("err = %v, want No slots", err)
	}
}

func TestAuthErrorIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeJSON(w, 401, map[string]any{"detail": "unauthorized"})
	})
	_, err := client.Get(context.Background(), KindTorrent, 1)
	if StatusOf(err) != 401 || !strings.Contains(err.Error(), "TorBox HTTP 401: unauthorized") {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

func TestServerErrorIsRetried(t *testing.T) {
	var hits atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			writeJSON(w, 502, map[string]any{"detail": "bad gateway"})
			return
		}
		writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"id": 3, "name": "x"}})
	})
	item, err := client.Get(context.Background(), KindTorrent, 3)
	if err != nil || item.ID != 3 {
		t.Fatalf("Get = %+v, %v", item, err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}

func TestErrorDetailShapes(t *testing.T) {
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"detail": "plain"}, "plain"},
		{map[string]any{"error": "CODE"}, "CODE"},
		{map[string]any{"detail": []any{map[string]any{"msg": "a"}, "b"}}, "a; b"},
		{map[string]any{"detail": map[string]any{"message": "nested"}}, "nested"},
		{map[string]any{}, "fallback"},
	}
	for _, c := range cases {
		if got := errorDetail(c.body, "fallback"); got != c.want {
			t.Errorf("errorDetail(%v) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestNetworkErrorHidesAPIKey(t *testing.T) {
	client := New("secret-key", "http://127.0.0.1:1", time.Second)
	client.global = newBucket(1000, 1000)
	client.retryBase = time.Millisecond
	_, err := client.RequestLink(context.Background(), KindTorrent, 1, nil, true)
	if err == nil {
		t.Fatal("want an error from a closed port")
	}
	if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "token=") {
		t.Errorf("error leaks the request URL: %v", err)
	}
}

func TestListAllPaginates(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("bypass_cache") != "true" {
			t.Errorf("query = %v", r.URL.Query())
		}
		var data []any
		switch offset {
		case 0:
			for i := 1; i <= 100; i++ {
				data = append(data, map[string]any{"id": i, "name": fmt.Sprint(i), "download_state": "completed"})
			}
		case 100:
			data = append(data, map[string]any{"id": 1000, "name": "queued", "download_state": "queued"})
		}
		writeJSON(w, 200, map[string]any{"success": true, "data": data})
	})
	items, err := client.ListAll(context.Background(), KindUsenet)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(items) != 101 || items[100].ID != 1000 || items[100].Kind != KindUsenet {
		t.Errorf("got %d items, last %+v", len(items), items[len(items)-1])
	}
}

func TestListAllStopsWhenOffsetIsIgnored(t *testing.T) {
	var hits atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var data []any
		for i := 1; i <= 100; i++ {
			data = append(data, map[string]any{"id": i})
		}
		writeJSON(w, 200, map[string]any{"success": true, "data": data})
	})
	items, err := client.ListAll(context.Background(), KindTorrent)
	if err != nil || len(items) != 100 || hits.Load() != 2 {
		t.Errorf("items = %d, err = %v, hits = %d", len(items), err, hits.Load())
	}
}

func TestQueuedMapsKinds(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"success": true, "data": []any{
			map[string]any{"id": 201, "name": "t", "type": "torrent"},
			map[string]any{"id": 202, "name_override": "n", "type": "usenet"},
			map[string]any{"id": 203, "name": "w", "type": "webdl"},
		}})
	})
	items, err := client.Queued(context.Background())
	if err != nil {
		t.Fatalf("Queued: %v", err)
	}
	want := []struct {
		id         int64
		kind, name string
	}{{201, KindTorrent, "t"}, {202, KindUsenet, "n"}, {203, KindWebDL, "w"}}
	for i, w := range want {
		got := items[i]
		if got.ID != w.id || got.Kind != w.kind || got.Name != w.name || got.State != "queued" || !got.IsActive() {
			t.Errorf("item %d = %+v", i, got)
		}
	}
}

func TestDeleteAllUsesBulkControlEndpoint(t *testing.T) {
	cases := map[string]string{
		KindTorrent:      "/api/torrents/controltorrent",
		KindUsenet:       "/api/usenet/controlusenetdownload",
		KindWebDL:        "/api/webdl/controlwebdownload",
		CollectionQueued: "/api/queued/controlqueued",
	}
	for collection, path := range cases {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != path || string(body) != `{"all":true,"operation":"delete"}` {
				t.Errorf("%s: got %s %s", collection, r.URL.Path, body)
			}
			writeJSON(w, 200, map[string]any{"success": true, "data": nil})
		})
		if err := client.DeleteAll(context.Background(), collection); err != nil {
			t.Errorf("DeleteAll(%s): %v", collection, err)
		}
	}
	if err := New("k", "", time.Second).DeleteAll(context.Background(), "other"); err == nil {
		t.Error("DeleteAll accepted an unknown collection")
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"success": true, "data": []any{}})
	})
	_, err := client.Get(context.Background(), KindWebDL, 5)
	if !IsNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
}

func TestCreateQuotaSoftStops(t *testing.T) {
	quota := newCreateQuota()
	for i := 0; i < createsPerHour-createHeadroom; i++ {
		if err := quota.check(KindWebDL); err != nil {
			t.Fatalf("create %d refused: %v", i, err)
		}
		quota.record(KindWebDL)
	}
	err := quota.check(KindWebDL)
	if StatusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a soft 429", err)
	}
	// Kinds have separate budgets.
	if err := quota.check(KindTorrent); err != nil {
		t.Errorf("torrent refused: %v", err)
	}
	// An hour on, the window has emptied.
	quota.stamps[KindWebDL][0] = time.Now().Add(-2 * time.Hour)
	if err := quota.check(KindWebDL); err != nil {
		t.Errorf("expired create still counted: %v", err)
	}
}

func TestBucketWaitHonoursContext(t *testing.T) {
	b := newBucket(0.01, 1)
	if err := b.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait = %v, want deadline exceeded", err)
	}
}

func TestParseItemReadsTelemetry(t *testing.T) {
	item := parseItem(map[string]any{
		"usenet_id": float64(4), "title": "T", "size": float64(1000), "downloadState": "downloading",
		"progress": 0.5, "total_downloaded": float64(400), "download_speed": float64(10), "eta": float64(30),
		"files": []any{map[string]any{"file_id": float64(8), "short_name": "a.mkv", "size": float64(5)}},
	}, KindUsenet)
	if item.ID != 4 || item.Name != "T" || item.State != "downloading" || item.Percent() != 50 {
		t.Errorf("item = %+v", item)
	}
	if !item.HasDownloaded || item.Downloaded != 400 || !item.HasETA || item.ETA != 30 {
		t.Errorf("telemetry = %+v", item)
	}
	if len(item.Files) != 1 || !item.Files[0].HasID || item.Files[0].ID != 8 || item.Files[0].Name != "a.mkv" {
		t.Errorf("files = %+v", item.Files)
	}
	bare := parseItem(map[string]any{"id": float64(1)}, KindTorrent)
	if bare.HasProgress || bare.HasETA || bare.HasDownloaded || bare.Name != "unknown" {
		t.Errorf("missing telemetry was invented: %+v", bare)
	}
}
