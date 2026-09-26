package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/config"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/store"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		text        string
		wantCommand string
		wantArg     string
		wantPayload string
		wantOK      bool
	}{
		{"/torrent magnet:?xt=urn:btih:abc", "torrent", "magnet:?xt=urn:btih:abc", "magnet:?xt=urn:btih:abc", true},
		{"/status@MyTorBot 42", "status", "42", "42", true},
		{"/START", "start", "", "", true},
		{"  /status  ", "status", "", "", true},
		{"hello", "", "", "", false},
		{"/", "", "", "", false},
		{"/@MyTorBot", "", "", "", false},
		// A newline after the command still carries the payload: this is how
		// a list of NZB IDs pastes out of a search result.
		{"/nzb\nAAAA\nBBBB", "nzb", "AAAA", "AAAA\nBBBB", true},
		{"/nzbsearch\nMarvel's Spider-Man 2", "nzbsearch", "Marvel's", "Marvel's Spider-Man 2", true},
		// Mention and case are both normalized away.
		{"/STATUS@MyTorBot 42", "status", "42", "42", true},
		// The mention is matched case-insensitively, as Telegram treats it.
		{"/status@mytorbot 42", "status", "42", "42", true},
		// Addressed to a different bot in the group: not ours to answer.
		{"/help@SomeOtherBot", "", "", "", false},
		{"/logs@cosmosusenetbot", "", "", "", false},
	}

	b := &Bot{username: "MyTorBot"}

	for _, tc := range cases {
		command, args, payload, ok := b.parseCommand(tc.text)
		if ok != tc.wantOK {
			t.Errorf("parseCommand(%q) ok = %v, want %v", tc.text, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if command != tc.wantCommand {
			t.Errorf("parseCommand(%q) command = %q, want %q", tc.text, command, tc.wantCommand)
		}
		arg := ""
		if len(args) > 0 {
			arg = args[0]
		}
		if arg != tc.wantArg {
			t.Errorf("parseCommand(%q) first arg = %q, want %q", tc.text, arg, tc.wantArg)
		}
		if payload != tc.wantPayload {
			t.Errorf("parseCommand(%q) payload = %q, want %q", tc.text, payload, tc.wantPayload)
		}
	}
}

func TestParseMagnet(t *testing.T) {
	magnet, hash := parseMagnet("magnet:?xt=urn:btih:ABCDEF1234567890ABCDEF1234567890ABCDEF12&dn=x")
	if !strings.HasPrefix(magnet, "magnet:") || hash != "abcdef1234567890abcdef1234567890abcdef12" {
		t.Errorf("magnet = %q, hash = %q", magnet, hash)
	}
	magnet, hash = parseMagnet("ABCDEF1234567890ABCDEF1234567890ABCDEF12")
	if magnet != "magnet:?xt=urn:btih:abcdef1234567890abcdef1234567890abcdef12" || hash == "" {
		t.Errorf("bare hash: magnet = %q, hash = %q", magnet, hash)
	}
	if magnet, _ := parseMagnet("magnet:?xt=urn:btih:MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U"); magnet == "" {
		t.Error("base32 magnet refused")
	}
	if magnet, _ := parseMagnet("not a magnet"); magnet != "" {
		t.Errorf("garbage accepted as %q", magnet)
	}
}

func TestMagnetName(t *testing.T) {
	named := "magnet:?xt=urn:btih:B2282CA046DEEFB6E59FA46BE5C4909C4223E5FC&dn=Project+Hail+Mary+%282026%29+%5BQxR%5D"
	if got := magnetName(named); got != "Project Hail Mary (2026) [QxR]" {
		t.Errorf("magnetName = %q", got)
	}
	if got := magnetName("magnet:?xt=urn:btih:abc"); got != "" {
		t.Errorf("magnetName without dn = %q", got)
	}
}

func TestParseSearchArgs(t *testing.T) {
	cases := []struct {
		in, query, sort string
		wantErr         bool
	}{
		{"ubuntu server", "ubuntu server", "", false},
		{"ubuntu --mx", "ubuntu", "max", false},
		{"ubuntu --mn", "ubuntu", "min", false},
		{`"Marvel's Spider-Man 2" --mx`, "Marvel's Spider-Man 2", "max", false},
		{"--mx ubuntu", "", "", true},
		{"ubuntu --mx --mn", "", "", true},
		{"ubuntu --mx --mx", "", "", true},
	}
	for _, c := range cases {
		query, sort, parseErr := parseSearchArgs(c.in)
		if (parseErr != "") != c.wantErr || query != c.query || sort != c.sort {
			t.Errorf("parseSearchArgs(%q) = %q, %q, %q", c.in, query, sort, parseErr)
		}
	}
}

func TestDecodeNZBID(t *testing.T) {
	// Produced by the Python bot's nzb_ids.encode, which the NZBGet bot's IDs
	// also match.
	cases := map[string]string{
		"PP1qP7iqyIXy3sokT5k=":                     "12345",
		"IPhsPLnulM2igYd3C8hcb6x2AK+sSe6s6iqeQRQ=": "-7574822274760567514",
		// A bare Hydra result ID works too.
		"-7574822274760567514": "-7574822274760567514",
	}
	for token, want := range cases {
		if got, err := decodeNZBID(token); err != nil || got != want {
			t.Errorf("decodeNZBID(%q) = %q, %v; want %q", token, got, err, want)
		}
	}
	if got := nzbID("12345"); got != "PP1qP7iqyIXy3sokT5k=" {
		t.Errorf("nzbID = %q", got)
	}
	for _, bad := range []string{"not base64!!", "bK06", "AAAAAAAAAAAAAAAA"} {
		if _, err := decodeNZBID(bad); !errors.Is(err, errInvalidNZBID) {
			t.Errorf("decodeNZBID(%q) err = %v, want errInvalidNZBID", bad, err)
		}
	}
}

func TestNameKey(t *testing.T) {
	if nameKey("Some.Release-GRP.nzb") != nameKey("some release grp") {
		t.Error("nameKey should ignore case, separators and .nzb")
	}
}

func TestAddedText(t *testing.T) {
	for kind, title := range addedTitles {
		got := addedText(kind, "The.Ring.2002.mkv", 12345, "Cheem<kandi", "")
		want := "<u><b>" + title + "</b></u>\n\n<code>The.Ring.2002.mkv</code>\n\n" +
			`by <a href="tg://user?id=12345">Cheem&lt;kandi</a>`
		if got != want {
			t.Errorf("addedText(%s) = %q, want %q", kind, got, want)
		}
	}
	linked := addedText(torbox.KindTorrent, "Project Hail Mary (2026).mkv", 1, "A", "https://dl.example/d/A&B")
	if !strings.Contains(linked, `<a href="https://dl.example/d/A&amp;B"><code>Project Hail Mary (2026).mkv</code></a>`) {
		t.Errorf("linked = %q", linked)
	}
}

func TestTorboxErrorTextHints(t *testing.T) {
	got := torboxErrorText("TorBox HTTP 400: Download is not ready <yet>")
	for _, want := range []string{"&lt;yet&gt;", "still be downloading"} {
		if !strings.Contains(got, want) {
			t.Errorf("torboxErrorText = %q, missing %q", got, want)
		}
	}
	if got := torboxErrorText("TorBox HTTP 401: bad token"); !strings.Contains(got, "TorBox API key") {
		t.Errorf("auth hint missing: %q", got)
	}
}

func TestChannelTexts(t *testing.T) {
	got := channelSuccessText("File.Name.2026.1080p", 42, 9073741824, "https://dl.example/d/x&y")
	want := "<code>File.Name.2026.1080p</code> - [<code>42</code>]\n\n" +
		`<b>SUCCESS • 8.45 GB • <a href="https://dl.example/d/x&amp;y">DL</a></b>`
	if got != want {
		t.Errorf("success = %q, want %q", got, want)
	}
	if got := channelFailedText("A<b>", 1); got != "<code>A&lt;b&gt;</code> - [<code>1</code>]\n\n<b>FAILED</b>" {
		t.Errorf("failed = %q", got)
	}
}

func TestCooldown(t *testing.T) {
	c := newCooldowns()
	if wait := c.take(1); wait != 0 {
		t.Fatalf("first add waited %v", wait)
	}
	if wait := c.take(1); wait <= 0 || wait > createCooldown {
		t.Errorf("second add wait = %v", wait)
	}
	if wait := c.take(2); wait != 0 {
		t.Errorf("another user waited %v", wait)
	}
}

func TestChannelQueuesEveryKind(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := &Bot{cfg: &config.Bot{DownloadChannelID: -1001234567890}, store: db, log: slog.New(slog.DiscardHandler)}
	b.channel = newChannelPublisher(b)

	for i, kind := range torbox.Kinds {
		b.channel.enqueue(1, &torbox.Link{Kind: kind, ID: int64(i + 1), Name: kind})
	}
	jobs, err := db.PendingJobs()
	if err != nil || len(jobs) != len(torbox.Kinds) {
		t.Fatalf("PendingJobs = %+v, %v; want one per kind", jobs, err)
	}
	if jobs[2].Kind != torbox.KindWebDL {
		t.Errorf("web download not queued: %+v", jobs)
	}
}

func TestStillRunningSkipsCachedDownloads(t *testing.T) {
	states := map[string]string{"1": "cached", "2": "downloading"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"success":true,"data":{"id":%s,"download_state":%q}}`,
			r.URL.Query().Get("id"), states[r.URL.Query().Get("id")])
	}))
	defer server.Close()
	b := &Bot{torbox: torbox.New("k", server.URL, 5*time.Second)}
	ctx := context.Background()

	// A link handed over at once means TorBox had it cached: no lookup needed.
	if b.stillRunning(ctx, &torbox.Link{Kind: torbox.KindTorrent, ID: 2, URL: "https://cdn.example/x"}) {
		t.Error("download with a ready link reported as running")
	}
	if b.stillRunning(ctx, &torbox.Link{Kind: torbox.KindUsenet, ID: 1}) {
		t.Error("cached download reported as running")
	}
	if !b.stillRunning(ctx, &torbox.Link{Kind: torbox.KindWebDL, ID: 2}) {
		t.Error("downloading item reported as done")
	}
}
