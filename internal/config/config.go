// Package config loads and validates bot configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// DefaultDatabasePath is used when DATABASE_PATH is not set.
const DefaultDatabasePath = "torbot.db"

// DefaultHTTPTimeout bounds one TorBox request when HTTP_TIMEOUT is not set.
const DefaultHTTPTimeout = 30 * time.Second

// MinProxySecret is the shortest PROXY_SECRET the Worker accepts.
const MinProxySecret = 16

// Proxy modes, matching the Worker's token "m" claim.
const (
	ProxyModeWebDAV = "webdav"
	ProxyModeAPI    = "api"
	ProxyModeCDN    = "cdn"
)

// Error is returned when required environment configuration is missing or invalid.
type Error struct {
	msg string
}

func (e *Error) Error() string { return e.msg }

func configErrorf(format string, args ...any) error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// Bot holds every setting the bot needs to run.
type Bot struct {
	TelegramAPIID     int
	TelegramAPIHash   string
	TelegramBotToken  string
	OwnerID           int64
	AuthorizedChatIDs []int64

	TorBoxAPIKey string
	// HTTPTimeout bounds one request to TorBox or NZBHydra.
	HTTPTimeout time.Duration

	// DatabasePath is the SQLite file holding runtime authorizations and the
	// channel publication queue.
	DatabasePath string

	// DownloadChannelID is the Bot API style ID (-100...) of the channel that
	// finished downloads are posted to. Zero disables the channel.
	DownloadChannelID int64

	// Cloudflare Worker proxy that hides TorBox CDN and WebDAV URLs.
	ProxyBaseURL string
	ProxySecret  string
	ProxyMode    string
	ProxyTTL     time.Duration
	ProxyCDNTTL  time.Duration
	// ProxyPageTTL is how long a file-list page link stays valid. It is what
	// the channel posts, so it outlives the single-file links it opens.
	ProxyPageTTL       time.Duration
	ProxyWebDAVFlatten bool
	ProxySingleUse     bool

	// NZBHydraURL is the Hydra base URL, basic-auth credentials included and
	// any trailing /api removed. Empty disables /nzbsearch and adding NZBs by ID.
	NZBHydraURL            string
	NZBHydraDownloaderName string
	SearchResultsPerPage   int
	// SearchRedact is how long a search result message stays visible before it
	// is redacted automatically. Zero keeps results until CLOSE is tapped.
	SearchRedact time.Duration
}

// IsAuthorizedChat reports whether the chat is allowlisted in .env.
func (c *Bot) IsAuthorizedChat(chatID int64) bool {
	for _, id := range c.AuthorizedChatIDs {
		if id == chatID {
			return true
		}
	}
	return false
}

// ProxyEnabled reports whether download links go through the Worker.
func (c *Bot) ProxyEnabled() bool {
	return c.ProxyBaseURL != "" && len(c.ProxySecret) >= MinProxySecret
}

// NZBHydraEnabled reports whether NZB search is configured.
func (c *Bot) NZBHydraEnabled() bool { return c.NZBHydraURL != "" }

// ChannelEnabled reports whether finished downloads are posted to a channel.
func (c *Bot) ChannelEnabled() bool { return c.DownloadChannelID != 0 }

func clean(value string) string { return strings.TrimSpace(value) }

func env(name string) string { return clean(os.Getenv(name)) }

func parseAuthorizedChatIDs(value string) ([]int64, error) {
	if value == "" {
		return nil, nil
	}

	var chatIDs []int64
	for _, part := range strings.Split(strings.ReplaceAll(value, "\n", ","), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, configErrorf("AUTHORIZED_CHAT_IDS must be a comma-separated list of chat IDs")
		}
		chatIDs = append(chatIDs, id)
	}
	return chatIDs, nil
}

// parseBool reads a 0/1 style switch, falling back when unset. Anything else is
// rejected rather than guessed at: "5" silently meaning off is how a setting
// stays disabled without anyone noticing.
func parseBool(name, value string, fallback bool) (bool, error) {
	switch strings.ToLower(value) {
	case "":
		return fallback, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, configErrorf("%s must be 1 or 0", name)
}

// parseSeconds reads a whole number of seconds, at least min.
func parseSeconds(name, value string, fallback time.Duration, min int) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < min {
		return 0, configErrorf("%s must be a whole number of seconds, at least %d", name, min)
	}
	return time.Duration(seconds) * time.Second, nil
}

func parseResultsPerPage(value string) (int, error) {
	if value == "" {
		return 5, nil
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 1 || count > 10 {
		return 0, configErrorf("NZBSEARCH_RESULTS_PER_PAGE must be a whole number from 1 to 10")
	}
	return count, nil
}

// parseChannelID accepts only a Bot API channel ID: a channel is the one kind
// of chat the bot can post to without anyone having messaged it first.
func parseChannelID(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id > -1000000000000 {
		return 0, configErrorf("DOWNLOAD_CHANNEL_ID must be a channel ID such as -1001234567890")
	}
	return id, nil
}

func parseProxyMode(value string) (string, error) {
	switch mode := strings.ToLower(value); mode {
	case "":
		return ProxyModeWebDAV, nil
	case ProxyModeWebDAV, ProxyModeAPI, ProxyModeCDN:
		return mode, nil
	}
	return "", configErrorf("PROXY_MODE must be webdav, api or cdn")
}

// parseProxyBaseURL keeps the Worker origin and any path prefix, without a
// trailing slash, so links come out as <base>/d/<token>.
func parseProxyBaseURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", configErrorf("PROXY_BASE_URL must be an absolute URL, such as https://torbot-dl.example.workers.dev")
	}
	return strings.TrimRight(value, "/"), nil
}

// ParseNZBHydraURL strips a trailing slash and /api so either form can be
// configured. A suffix trim, not a character-set trim: cutting the set "/api"
// over-trims a path that merely ends in those letters.
func ParseNZBHydraURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", configErrorf("NZBHYDRA_URL must be an absolute URL, such as https://user:password@hydra.example.com")
	}
	base := strings.TrimRight(value, "/")
	base = strings.TrimSuffix(base, "/api")
	return strings.TrimRight(base, "/"), nil
}

// Load reads .env plus the process environment and returns a validated config.
func Load() (*Bot, error) {
	// A missing .env is fine when the environment is already populated.
	_ = godotenv.Load()

	telegramAPIIDRaw := env("TELEGRAM_API_ID")
	telegramAPIHash := env("TELEGRAM_API_HASH")
	telegramBotToken := env("TELEGRAM_BOT_TOKEN")
	ownerIDRaw := env("OWNER_ID")
	torboxAPIKey := env("TORBOX_API_KEY")

	if telegramAPIIDRaw == "" {
		return nil, configErrorf("Missing TELEGRAM_API_ID in .env")
	}
	if telegramAPIHash == "" {
		return nil, configErrorf("Missing TELEGRAM_API_HASH in .env")
	}
	if telegramBotToken == "" {
		return nil, configErrorf("Missing TELEGRAM_BOT_TOKEN in .env")
	}
	if ownerIDRaw == "" {
		return nil, configErrorf("Missing OWNER_ID in .env")
	}
	if torboxAPIKey == "" {
		return nil, configErrorf("Missing TORBOX_API_KEY in .env")
	}

	telegramAPIID, err := strconv.Atoi(telegramAPIIDRaw)
	if err != nil {
		return nil, configErrorf("TELEGRAM_API_ID must be an integer")
	}

	ownerID, err := strconv.ParseInt(ownerIDRaw, 10, 64)
	if err != nil {
		return nil, configErrorf("OWNER_ID must be a Telegram numeric user ID")
	}

	cfg := &Bot{
		TelegramAPIID:    telegramAPIID,
		TelegramAPIHash:  telegramAPIHash,
		TelegramBotToken: telegramBotToken,
		OwnerID:          ownerID,
		TorBoxAPIKey:     torboxAPIKey,
		ProxySecret:      env("PROXY_SECRET"),
	}

	if cfg.AuthorizedChatIDs, err = parseAuthorizedChatIDs(env("AUTHORIZED_CHAT_IDS")); err != nil {
		return nil, err
	}

	cfg.DatabasePath = env("DATABASE_PATH")
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = DefaultDatabasePath
	}
	if cfg.HTTPTimeout, err = parseSeconds("HTTP_TIMEOUT", env("HTTP_TIMEOUT"), DefaultHTTPTimeout, 1); err != nil {
		return nil, err
	}
	if cfg.DownloadChannelID, err = parseChannelID(env("DOWNLOAD_CHANNEL_ID")); err != nil {
		return nil, err
	}

	if cfg.ProxyBaseURL, err = parseProxyBaseURL(env("PROXY_BASE_URL")); err != nil {
		return nil, err
	}
	if cfg.ProxyBaseURL != "" && len(cfg.ProxySecret) < MinProxySecret {
		return nil, configErrorf("PROXY_SECRET must be at least %d characters when PROXY_BASE_URL is set", MinProxySecret)
	}
	if cfg.ProxyMode, err = parseProxyMode(env("PROXY_MODE")); err != nil {
		return nil, err
	}
	if cfg.ProxyTTL, err = parseSeconds("PROXY_TTL_SECONDS", env("PROXY_TTL_SECONDS"), time.Hour, 60); err != nil {
		return nil, err
	}
	if cfg.ProxyCDNTTL, err = parseSeconds("PROXY_CDN_TTL_SECONDS", env("PROXY_CDN_TTL_SECONDS"), 15*time.Minute, 60); err != nil {
		return nil, err
	}
	if cfg.ProxyPageTTL, err = parseSeconds("PROXY_PAGE_TTL_SECONDS", env("PROXY_PAGE_TTL_SECONDS"), 24*time.Hour, 60); err != nil {
		return nil, err
	}
	// The channel only ever posts Worker links; a raw TorBox CDN URL in a
	// channel is exactly the private-link sharing the ToS forbids.
	if cfg.ChannelEnabled() && !cfg.ProxyEnabled() {
		return nil, configErrorf("DOWNLOAD_CHANNEL_ID needs PROXY_BASE_URL and PROXY_SECRET")
	}

	switches := []struct {
		name     string
		fallback bool
		target   *bool
	}{
		{"PROXY_WEBDAV_FLATTEN", false, &cfg.ProxyWebDAVFlatten},
		{"PROXY_SINGLE_USE", true, &cfg.ProxySingleUse},
	}
	for _, s := range switches {
		if *s.target, err = parseBool(s.name, env(s.name), s.fallback); err != nil {
			return nil, err
		}
	}

	if cfg.NZBHydraURL, err = ParseNZBHydraURL(env("NZBHYDRA_URL")); err != nil {
		return nil, err
	}
	cfg.NZBHydraDownloaderName = env("NZBHYDRA_DOWNLOADER_NAME")
	if cfg.NZBHydraDownloaderName == "" {
		cfg.NZBHydraDownloaderName = "TorBox"
	}
	if cfg.SearchResultsPerPage, err = parseResultsPerPage(env("NZBSEARCH_RESULTS_PER_PAGE")); err != nil {
		return nil, err
	}
	if cfg.SearchRedact, err = parseSeconds("NZBSEARCH_AUTOREDACT", env("NZBSEARCH_AUTOREDACT"), 0, 0); err != nil {
		return nil, err
	}

	return cfg, nil
}
