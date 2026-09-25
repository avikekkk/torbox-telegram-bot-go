package config

import (
	"strings"
	"testing"
	"time"
)

// setRequired fills in every required setting, and blanks the optional ones a
// developer's own environment might carry.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("TELEGRAM_API_ID", "123")
	t.Setenv("TELEGRAM_API_HASH", "hash")
	t.Setenv("TELEGRAM_BOT_TOKEN", "1:token")
	t.Setenv("OWNER_ID", "42")
	t.Setenv("TORBOX_API_KEY", "key")
	for _, name := range []string{
		"AUTHORIZED_CHAT_IDS", "DATABASE_PATH", "HTTP_TIMEOUT", "DOWNLOAD_CHANNEL_ID", "PROXY_BASE_URL",
		"PROXY_SECRET", "PROXY_MODE", "PROXY_TTL_SECONDS", "PROXY_CDN_TTL_SECONDS", "PROXY_PAGE_TTL_SECONDS", "PROXY_WEBDAV_FLATTEN",
		"PROXY_SINGLE_USE", "NZBHYDRA_URL", "NZBHYDRA_DOWNLOADER_NAME",
		"NZBSEARCH_RESULTS_PER_PAGE", "NZBSEARCH_AUTOREDACT",
	} {
		t.Setenv(name, "")
	}
	// A .env next to the tests must not leak in.
	t.Chdir(t.TempDir())
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OwnerID != 42 || cfg.DatabasePath != DefaultDatabasePath || cfg.HTTPTimeout != DefaultHTTPTimeout {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.ProxyMode != ProxyModeWebDAV || cfg.ProxyTTL != time.Hour || cfg.ProxyCDNTTL != 15*time.Minute {
		t.Errorf("proxy defaults = %q %v %v", cfg.ProxyMode, cfg.ProxyTTL, cfg.ProxyCDNTTL)
	}
	if cfg.ProxyPageTTL != 24*time.Hour {
		t.Errorf("page link TTL = %v, want 24h", cfg.ProxyPageTTL)
	}
	if !cfg.ProxySingleUse || cfg.ProxyWebDAVFlatten {
		t.Errorf("proxy switches = single-use %v, flatten %v", cfg.ProxySingleUse, cfg.ProxyWebDAVFlatten)
	}
	if cfg.ProxyEnabled() || cfg.NZBHydraEnabled() || cfg.ChannelEnabled() {
		t.Error("optional features should default off")
	}
	if cfg.NZBHydraDownloaderName != "TorBox" || cfg.SearchResultsPerPage != 5 || cfg.SearchRedact != 0 {
		t.Errorf("search defaults = %q %d %v", cfg.NZBHydraDownloaderName, cfg.SearchResultsPerPage, cfg.SearchRedact)
	}
}

func TestLoadReportsMissingRequired(t *testing.T) {
	for _, name := range []string{"TELEGRAM_API_ID", "TELEGRAM_API_HASH", "TELEGRAM_BOT_TOKEN", "OWNER_ID", "TORBOX_API_KEY"} {
		setRequired(t)
		t.Setenv(name, "")
		_, err := Load()
		if err == nil || err.Error() != "Missing "+name+" in .env" {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"TELEGRAM_API_ID", "abc", "TELEGRAM_API_ID"},
		{"OWNER_ID", "me", "OWNER_ID"},
		{"AUTHORIZED_CHAT_IDS", "1,two", "AUTHORIZED_CHAT_IDS"},
		{"DOWNLOAD_CHANNEL_ID", "-12345", "DOWNLOAD_CHANNEL_ID"},
		{"PROXY_MODE", "ftp", "PROXY_MODE"},
		{"PROXY_BASE_URL", "worker.dev", "PROXY_BASE_URL"},
		{"NZBHYDRA_URL", "hydra:5076", "NZBHYDRA_URL"},
		{"NZBSEARCH_RESULTS_PER_PAGE", "11", "NZBSEARCH_RESULTS_PER_PAGE"},
		{"NZBSEARCH_AUTOREDACT", "-1", "NZBSEARCH_AUTOREDACT"},
		// "5" is neither on nor off, so it is refused rather than guessed at.
		{"PROXY_SINGLE_USE", "5", "PROXY_SINGLE_USE"},
	}
	for _, c := range cases {
		setRequired(t)
		t.Setenv(c.name, c.value)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s=%q: err = %v", c.name, c.value, err)
		}
	}
}

func TestProxyNeedsLongSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("PROXY_BASE_URL", "https://dl.example.workers.dev/")
	t.Setenv("PROXY_SECRET", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PROXY_SECRET") {
		t.Fatalf("short secret: err = %v", err)
	}

	t.Setenv("PROXY_SECRET", "0123456789abcdef")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProxyEnabled() || cfg.ProxyBaseURL != "https://dl.example.workers.dev" {
		t.Errorf("proxy = %v %q", cfg.ProxyEnabled(), cfg.ProxyBaseURL)
	}
}

func TestChannelNeedsProxy(t *testing.T) {
	setRequired(t)
	t.Setenv("DOWNLOAD_CHANNEL_ID", "-1004452601845")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PROXY_BASE_URL") {
		t.Fatalf("channel without proxy: err = %v", err)
	}

	t.Setenv("PROXY_BASE_URL", "https://dl.example.workers.dev")
	t.Setenv("PROXY_SECRET", "0123456789abcdef")
	cfg, err := Load()
	if err != nil || cfg.DownloadChannelID != -1004452601845 {
		t.Fatalf("Load = %v, %v", cfg, err)
	}
}

func TestParseAuthorizedChatIDs(t *testing.T) {
	ids, err := parseAuthorizedChatIDs("-1001234567890, 341042533,\n7611465677")
	if err != nil || len(ids) != 3 || ids[0] != -1001234567890 || ids[2] != 7611465677 {
		t.Errorf("ids = %v, err = %v", ids, err)
	}
}

func TestParseNZBHydraURL(t *testing.T) {
	cases := map[string]string{
		"https://u:p@hydra.example.com":          "https://u:p@hydra.example.com",
		"https://u:p@hydra.example.com/":         "https://u:p@hydra.example.com",
		"https://u:p@hydra.example.com/api":      "https://u:p@hydra.example.com",
		"https://hydra.example.com/nzbhydra/api": "https://hydra.example.com/nzbhydra",
		// A path that merely ends in the letters is not an /api suffix.
		"https://hydra.example.com/tapi": "https://hydra.example.com/tapi",
	}
	for in, want := range cases {
		if got, err := ParseNZBHydraURL(in); err != nil || got != want {
			t.Errorf("ParseNZBHydraURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
