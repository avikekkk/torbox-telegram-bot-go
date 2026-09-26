// Package proxy builds links to the Cloudflare Worker in workers/torbox-proxy,
// which streams TorBox downloads so the TorBox CDN URLs are never shown.
//
// A link is <base>/d/<token>, where the token is
//
//	base64url( 0x02 || iv(12) || AES-GCM(json claims) )
//
// keyed with SHA-256(PROXY_SECRET). The claims are encrypted, not just signed,
// so a CDN URL cannot be read back out of the link.
//
// The bot only makes list links: a page of the download's files, each with its
// own api link minted by the Worker. A single-file download streams straight
// away, never as a zip.
package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"time"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/config"
)

// tokenVersion is the first byte of every token the Worker accepts.
const tokenVersion = 2

const ivLength = 12

// Builder turns TorBox downloads into Worker links.
type Builder struct {
	baseURL string
	secret  string
	pageTTL time.Duration
}

// New returns a builder, or nil when the proxy is not configured.
func New(cfg *config.Bot) *Builder {
	if !cfg.ProxyEnabled() {
		return nil
	}
	return &Builder{
		baseURL: cfg.ProxyBaseURL,
		secret:  cfg.ProxySecret,
		pageTTL: cfg.ProxyPageTTL,
	}
}

// Page links to the Worker's file-list page for a download. The Worker looks
// the files up when the page opens, so the link needs no CDN URL and works for
// as long as the download stays on TorBox, up to PROXY_PAGE_TTL_SECONDS. A
// channel post is opened by many people, and more than once.
func (b *Builder) Page(kind string, id int64, name string) string {
	if b == nil {
		return ""
	}
	now := time.Now().Unix()
	claims := map[string]any{
		"v":   1,
		"exp": now + int64(b.pageTTL.Seconds()),
		"iat": now,
		"m":   "list",
		"k":   kind,
		"id":  id,
	}
	setName(claims, name)
	return b.seal(claims)
}

// nameUnsafe drops what would break the Content-Disposition header the Worker
// builds from the name.
var nameUnsafe = regexp.MustCompile(`[\r\n"]`)

func setName(claims map[string]any, name string) {
	if name == "" {
		return
	}
	name = nameUnsafe.ReplaceAllString(name, "")
	if runes := []rune(name); len(runes) > 120 {
		name = string(runes[:120])
	}
	claims["n"] = name
}

func (b *Builder) seal(claims map[string]any) string {
	token, err := Encrypt(claims, b.secret)
	if err != nil {
		return ""
	}
	return b.baseURL + "/d/" + url.PathEscape(token)
}

// Encrypt seals claims into a Worker token.
func Encrypt(claims map[string]any, secret string) (string, error) {
	if _, ok := claims["exp"]; !ok {
		return "", errors.New("claims must include exp")
	}
	aead, err := newAEAD(secret)
	if err != nil {
		return "", err
	}
	plaintext, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	iv := make([]byte, ivLength)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	blob := append([]byte{tokenVersion}, iv...)
	blob = aead.Seal(blob, iv, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(blob), nil
}

// Decrypt opens a token and checks it has not expired. It exists for tests;
// the Worker does this in production.
func Decrypt(token, secret string) (map[string]any, error) {
	blob, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	if len(blob) < 1+ivLength+16 || blob[0] != tokenVersion {
		return nil, errors.New("not a version 2 token")
	}
	aead, err := newAEAD(secret)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, blob[1:1+ivLength], blob[1+ivLength:], nil)
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(plaintext, &claims); err != nil {
		return nil, err
	}
	exp, ok := claims["exp"].(float64)
	if !ok || int64(exp) < time.Now().Unix() {
		return nil, errors.New("token expired")
	}
	return claims, nil
}

func newAEAD(secret string) (cipher.AEAD, error) {
	if len(secret) < config.MinProxySecret {
		return nil, errors.New("PROXY_SECRET must be at least 16 characters")
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
