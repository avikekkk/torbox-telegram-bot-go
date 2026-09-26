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

func newBuilder() *Builder {
	return New(&config.Bot{
		ProxyBaseURL: "https://dl.example.workers.dev",
		ProxySecret:  secret,
		ProxyPageTTL: time.Hour,
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

func TestPageLinkIsReusableAndLongLived(t *testing.T) {
	b := newBuilder()
	b.pageTTL = 7 * 24 * time.Hour
	claims := claimsOf(t, b.Page(torbox.KindUsenet, 2522511, "Crew.Girl.S01E06"))
	if claims["m"] != "list" || claims["k"] != "usenet" || claims["id"] != float64(2522511) || claims["n"] != "Crew.Girl.S01E06" {
		t.Errorf("claims = %v", claims)
	}
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	if left := time.Until(exp); left < 7*24*time.Hour-time.Hour || left > 7*24*time.Hour {
		t.Errorf("page link lives %v, want 7 days", left)
	}
	var disabled *Builder
	if disabled.Page(torbox.KindTorrent, 1, "x") != "" {
		t.Error("disabled builder made a page link")
	}
	if New(&config.Bot{}) != nil {
		t.Error("New without a proxy configured should be nil")
	}
}

func TestPrintPageTokens(t *testing.T) {
	out := os.Getenv("PAGE_TOKENS_OUT")
	if out == "" {
		t.Skip("set PAGE_TOKENS_OUT to write tokens for the Worker test")
	}
	b := newBuilder()
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
