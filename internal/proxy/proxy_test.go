package proxy

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/config"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

const secret = "0123456789abcdef-secret"

func newBuilder(mode string) *Builder {
	return New(&config.Bot{
		ProxyBaseURL:   "https://dl.example.workers.dev",
		ProxySecret:    secret,
		ProxyMode:      mode,
		ProxyTTL:       time.Hour,
		ProxyCDNTTL:    15 * time.Minute,
		ProxySingleUse: true,
	})
}

// claimsOf opens the token at the end of a Worker link.
func claimsOf(t *testing.T, link string) map[string]any {
	t.Helper()
	token, ok := strings.CutPrefix(link, "https://dl.example.workers.dev/d/")
	if !ok {
		t.Fatalf("link %q is not a Worker link", link)
	}
	claims, err := Decrypt(token, secret)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	return claims
}

func TestZipLinkWrapsTheCDNURL(t *testing.T) {
	link := newBuilder(config.ProxyModeWebDAV).Link(Target{
		Kind: torbox.KindTorrent, ID: 7, CDNURL: "https://cdn.torbox.example/x.zip", Zip: true,
		ItemName: `Pack "1"` + "\n", OwnerID: 99,
	})
	if strings.Contains(link, "cdn.torbox") {
		t.Fatalf("link leaks the CDN URL: %s", link)
	}
	claims := claimsOf(t, link)
	if claims["m"] != "cdn" || claims["u"] != "https://cdn.torbox.example/x.zip" || claims["n"] != "Pack 1" {
		t.Errorf("claims = %v", claims)
	}
	if claims["once"] != float64(1) || claims["uid"] != float64(99) || claims["jti"] == "" {
		t.Errorf("claims = %v", claims)
	}
	exp := int64(claims["exp"].(float64))
	if left := time.Until(time.Unix(exp, 0)); left < 14*time.Minute || left > 15*time.Minute {
		t.Errorf("cdn link lives %v, want the 15m CDN TTL", left)
	}
}

func TestWebDAVModeGuessesPaths(t *testing.T) {
	link := newBuilder(config.ProxyModeWebDAV).Link(Target{
		Kind: torbox.KindUsenet, ID: 3, ItemName: "Show.S01", FileName: "../ep1.mkv",
	})
	claims := claimsOf(t, link)
	if claims["m"] != "webdav" || claims["p"] != "/Show.S01/ep1.mkv" {
		t.Errorf("claims = %v", claims)
	}
	alt, _ := claims["alt"].([]any)
	if len(alt) != 3 || alt[0] != "/usenet/Show.S01/ep1.mkv" {
		t.Errorf("alt = %v", alt)
	}
}

func TestAPIModeCarriesTheID(t *testing.T) {
	link := newBuilder(config.ProxyModeAPI).Link(Target{Kind: torbox.KindWebDL, ID: 12, Zip: true, ItemName: "x"})
	claims := claimsOf(t, link)
	if claims["m"] != "api" || claims["k"] != "webdl" || claims["id"] != float64(12) || claims["z"] != float64(1) {
		t.Errorf("claims = %v", claims)
	}
}

func TestNoLinkWithoutSomethingToLinkTo(t *testing.T) {
	// A zip that is not ready has no CDN URL, and WebDAV cannot serve zips.
	if link := newBuilder(config.ProxyModeWebDAV).Link(Target{Kind: torbox.KindTorrent, ID: 1, Zip: true}); link != "" {
		t.Errorf("link = %q, want none", link)
	}
	// Only https CDN URLs are ever wrapped.
	if link := newBuilder(config.ProxyModeCDN).Link(Target{CDNURL: "http://10.0.0.1/x", Zip: true}); link != "" {
		t.Errorf("link = %q, want none", link)
	}
	var disabled *Builder
	if link := disabled.Link(Target{CDNURL: "https://cdn.example/x", Zip: true}); link != "" {
		t.Errorf("disabled builder made %q", link)
	}
	if New(&config.Bot{}) != nil {
		t.Error("New without a proxy configured should be nil")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	token, err := Encrypt(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "m": "cdn"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(token, secret+"x"); err == nil {
		t.Error("opened with the wrong secret")
	}
	expired, _ := Encrypt(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}, secret)
	if _, err := Decrypt(expired, secret); err == nil {
		t.Error("opened an expired token")
	}
	if _, err := Encrypt(map[string]any{"m": "cdn"}, secret); err == nil {
		t.Error("sealed claims without exp")
	}
}

func TestSanitizeSegment(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":    "etc passwd",
		"a\\b":                "a b",
		" .hidden. ":          "hidden",
		"tab\there\x00":       "tabhere",
		"Film  (2026)  1080p": "Film (2026) 1080p",
	}
	for in, want := range cases {
		if got := SanitizeSegment(in); got != want {
			t.Errorf("SanitizeSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebDAVPathsFlatten(t *testing.T) {
	got := WebDAVPaths("Item", "file.mkv", torbox.KindTorrent, true)
	want := []string{"/file.mkv", "/Item/file.mkv", "/torrents/Item/file.mkv", "/torrents/file.mkv"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("WebDAVPaths = %v, want %v", got, want)
	}
	if got := WebDAVPaths("", "", torbox.KindWebDL, false); len(got) != 0 {
		t.Errorf("WebDAVPaths(empty) = %v", got)
	}
}

func TestPageLinkIsReusableAndLongLived(t *testing.T) {
	b := newBuilder(config.ProxyModeWebDAV)
	b.pageTTL = 24 * time.Hour
	claims := claimsOf(t, b.Page(torbox.KindUsenet, 2522511, "Crew.Girl.S01E06"))
	if claims["m"] != "list" || claims["k"] != "usenet" || claims["id"] != float64(2522511) || claims["n"] != "Crew.Girl.S01E06" {
		t.Errorf("claims = %v", claims)
	}
	// Opened by everyone in the channel, and more than once.
	if _, once := claims["once"]; once {
		t.Errorf("page link is single-use: %v", claims)
	}
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	if left := time.Until(exp); left < 23*time.Hour || left > 24*time.Hour {
		t.Errorf("page link lives %v, want 24h", left)
	}
	var disabled *Builder
	if disabled.Page(torbox.KindTorrent, 1, "x") != "" {
		t.Error("disabled builder made a page link")
	}
}

func TestPrintPageTokens(t *testing.T) {
	out := os.Getenv("PAGE_TOKENS_OUT")
	if out == "" {
		t.Skip("set PAGE_TOKENS_OUT to write tokens for the Worker test")
	}
	b := newBuilder(config.ProxyModeWebDAV)
	b.pageTTL = time.Hour
	var lines []string
	for _, id := range []int64{1, 2, 3, 4} {
		link := b.Page(torbox.KindTorrent, id, "fallback")
		lines = append(lines, strings.TrimPrefix(link, "https://dl.example.workers.dev/d/"))
	}
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}
