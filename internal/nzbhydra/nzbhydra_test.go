package nzbhydra

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newHydra serves handler behind basic auth and Hydra's XSRF cookie, the way
// Hydra 9+ does.
func newHydra(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "u" || pass != "p" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/" {
			http.SetCookie(w, &http.Cookie{Name: xsrfCookie, Value: "xsrf-1", Path: "/"})
			return
		}
		if r.Header.Get("X-XSRF-TOKEN") != "xsrf-1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	base := strings.Replace(server.URL, "http://", "http://u:p@", 1)
	return New(base, 5*time.Second), server
}

func TestSearchKeepsEveryDigitOfTheResultID(t *testing.T) {
	client, _ := newHydra(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/internalapi/search" || body["query"] != "ubuntu" || body["category"] != "All" {
			t.Errorf("search request = %s %v", r.URL.Path, body)
		}
		// Numbers this large lose digits through float64.
		w.Write([]byte(`{"searchResults":[
			{"searchResultId":-7574822274760567514,"title":"<b>Ubuntu</b> 24.04","size":1073741824,"epoch":1700000000,"indexer":"x"},
			{"searchResultId":"123","title":"","size":"5"},
			{"searchResultId":"not-an-id","title":"skipped"}
		]}`))
	})

	results, err := client.Search(context.Background(), "ubuntu")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	first := results[0]
	if first.ID != "-7574822274760567514" || first.Title != "Ubuntu 24.04" || first.Size != 1073741824 {
		t.Errorf("first = %+v", first)
	}
	if first.Published.Unix() != 1700000000 || first.Indexer != "x" {
		t.Errorf("first = %+v", first)
	}
	if results[1].Title != "Untitled" || results[1].Size != 5 || !results[1].Published.IsZero() {
		t.Errorf("second = %+v", results[1])
	}
}

func TestAddReportsMissedIDs(t *testing.T) {
	var reply string
	client, _ := newHydra(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DownloaderName string              `json:"downloaderName"`
			SearchResults  []map[string]string `json:"searchResults"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPut || body.DownloaderName != "TorBox" || body.SearchResults[0]["searchResultId"] != "-5" {
			t.Errorf("add request = %s %+v", r.Method, body)
		}
		w.Write([]byte(reply))
	})

	reply = `{"successful":true,"missedIds":[]}`
	if err := client.Add(context.Background(), "-5", "TorBox"); err != nil {
		t.Errorf("Add: %v", err)
	}

	reply = `{"successful":true,"missedIds":[-5],"message":"gone"}`
	err := client.Add(context.Background(), "-5", "TorBox")
	if err == nil || !strings.Contains(err.Error(), "could not add it to TorBox: gone") {
		t.Errorf("missed: err = %v", err)
	}

	if err := client.Add(context.Background(), "../etc", "TorBox"); err == nil {
		t.Error("Add sent a malformed result ID")
	}
}

func TestErrorsNeverCarryCredentials(t *testing.T) {
	_, server := newHydra(t, func(w http.ResponseWriter, r *http.Request) {})
	wrong := New(strings.Replace(server.URL, "http://", "http://u:wrong@", 1), 5*time.Second)
	_, err := wrong.Search(context.Background(), "x")
	if err == nil || err.Error() != "NZBHydra rejected the credentials (HTTP 401)" {
		t.Errorf("wrong password: err = %v", err)
	}

	unreachable := New("http://u:secret@127.0.0.1:1", time.Second)
	_, err = unreachable.Search(context.Background(), "x")
	var hydraErr *Error
	if !errors.As(err, &hydraErr) || strings.Contains(err.Error(), "secret") {
		t.Errorf("unreachable: err = %v", err)
	}
}

func TestValidResultID(t *testing.T) {
	for _, id := range []string{"123", "-7574822274760567514"} {
		if !ValidResultID(id) {
			t.Errorf("ValidResultID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "abc", "1 2", "123456789012345678901"} {
		if ValidResultID(id) {
			t.Errorf("ValidResultID(%q) = true", id)
		}
	}
}
