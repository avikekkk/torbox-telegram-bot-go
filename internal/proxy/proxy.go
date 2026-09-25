// Package proxy builds links to the Cloudflare Worker in workers/torbox-proxy,
// which streams TorBox downloads so the TorBox CDN and WebDAV URLs are never
// shown.
//
// A link is <base>/d/<token>, where the token is
//
//	base64url( 0x02 || iv(12) || AES-GCM(json claims) )
//
// keyed with SHA-256(PROXY_SECRET). The claims are encrypted, not just signed,
// so a CDN URL cannot be read back out of the link.
//
// The Worker serves three kinds of claim:
//
//	webdav  stream a path from TorBox WebDAV (the Worker holds the API key)
//	api     call requestdl by kind and ID, then stream it (zips work too)
//	cdn     stream a short-lived CDN URL the bot already requested
//	list    show a page of the download's files, each with its own link; a
//	        single-file download streams straight away, never as a zip
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
	"strings"
	"time"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/config"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

// tokenVersion is the first byte of every token the Worker accepts.
const tokenVersion = 2

const ivLength = 12

// maxAltPaths bounds the WebDAV fallbacks a token carries.
const maxAltPaths = 8

// Builder turns TorBox downloads into Worker links.
type Builder struct {
	baseURL       string
	secret        string
	mode          string
	ttl           time.Duration
	cdnTTL        time.Duration
	pageTTL       time.Duration
	webdavFlatten bool
	singleUse     bool
}

// New returns a builder, or nil when the proxy is not configured.
func New(cfg *config.Bot) *Builder {
	if !cfg.ProxyEnabled() {
		return nil
	}
	return &Builder{
		baseURL:       cfg.ProxyBaseURL,
		secret:        cfg.ProxySecret,
		mode:          cfg.ProxyMode,
		ttl:           cfg.ProxyTTL,
		cdnTTL:        cfg.ProxyCDNTTL,
		pageTTL:       cfg.ProxyPageTTL,
		webdavFlatten: cfg.ProxyWebDAVFlatten,
		singleUse:     cfg.ProxySingleUse,
	}
}

// Target describes what a link should download.
type Target struct {
	Kind string
	ID   int64
	// CDNURL is the link TorBox returned, if one was requested.
	CDNURL string
	// Zip asks for every file as one archive; otherwise FileID picks one.
	Zip    bool
	FileID *int64
	// ItemName and FileName help the WebDAV mode guess a path.
	ItemName string
	FileName string
	// OwnerID ties the link to the Telegram user it was made for.
	OwnerID int64
}

// Link returns the best Worker link for target, or "" when none can be made.
func (b *Builder) Link(target Target) string {
	if b == nil {
		return ""
	}
	name := target.ItemName
	if name == "" {
		name = target.FileName
	}

	// A zip only exists as the CDN URL requestdl already produced.
	if target.Zip && target.CDNURL != "" {
		return b.cdnLink(target.CDNURL, name, target.OwnerID)
	}

	mode := b.mode
	if mode == config.ProxyModeWebDAV && !target.Zip {
		paths := WebDAVPaths(target.ItemName, target.FileName, target.Kind, b.webdavFlatten)
		if len(paths) > 0 {
			fileName := target.FileName
			if fileName == "" {
				fileName = target.ItemName
			}
			return b.webdavLink(paths[0], paths[1:], fileName, target.OwnerID)
		}
		mode = config.ProxyModeAPI
	}

	if mode == config.ProxyModeAPI {
		claims := b.claims(b.ttl, target.OwnerID)
		claims["m"] = "api"
		claims["k"] = target.Kind
		claims["id"] = target.ID
		if target.Zip {
			claims["z"] = 1
		} else if target.FileID != nil {
			claims["f"] = *target.FileID
		}
		setName(claims, name)
		if link := b.seal(claims); link != "" {
			return link
		}
	}

	if target.CDNURL != "" {
		return b.cdnLink(target.CDNURL, name, target.OwnerID)
	}
	return ""
}

// Page links to the Worker's file-list page for a download. The Worker looks
// the files up when the page opens, so the link needs no CDN URL and works for
// as long as the download stays on TorBox, up to PROXY_PAGE_TTL_SECONDS. It is
// never single-use: a channel post is opened by many people, and more than
// once.
func (b *Builder) Page(kind string, id int64, name string) string {
	if b == nil {
		return ""
	}
	claims := b.claims(b.pageTTL, 0)
	delete(claims, "once")
	claims["m"] = "list"
	claims["k"] = kind
	claims["id"] = id
	setName(claims, name)
	return b.seal(claims)
}

func (b *Builder) cdnLink(cdnURL, name string, ownerID int64) string {
	parsed, err := url.Parse(cdnURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	claims := b.claims(b.cdnTTL, ownerID)
	claims["m"] = "cdn"
	claims["u"] = cdnURL
	setName(claims, name)
	return b.seal(claims)
}

func (b *Builder) webdavLink(path string, alternatives []string, name string, ownerID int64) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.Contains(path, "..") || strings.HasPrefix(path, "//") {
		return ""
	}
	claims := b.claims(b.ttl, ownerID)
	claims["m"] = "webdav"
	claims["p"] = path
	var alt []string
	for _, candidate := range alternatives {
		if len(alt) == maxAltPaths {
			break
		}
		if !strings.HasPrefix(candidate, "/") {
			candidate = "/" + candidate
		}
		if !strings.Contains(candidate, "..") {
			alt = append(alt, candidate)
		}
	}
	if len(alt) > 0 {
		claims["alt"] = alt
	}
	setName(claims, name)
	return b.seal(claims)
}

// claims are the fields every token carries. jti is unique per link, so the
// Worker can refuse a single-use link the second time.
func (b *Builder) claims(ttl time.Duration, ownerID int64) map[string]any {
	now := time.Now().Unix()
	claims := map[string]any{
		"v":   1,
		"exp": now + int64(ttl.Seconds()),
		"iat": now,
		"jti": newJTI(),
	}
	if b.singleUse {
		claims["once"] = 1
	}
	if ownerID != 0 {
		claims["uid"] = ownerID
	}
	return claims
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

func newJTI() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

var (
	controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	spaces       = regexp.MustCompile(`\s+`)
)

// SanitizeSegment cleans one WebDAV path segment, so a download name cannot
// climb out of the WebDAV root.
func SanitizeSegment(name string) string {
	s := strings.ReplaceAll(name, "\\", "/")
	s = controlChars.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "/", " ")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", "")
	}
	s = strings.Trim(strings.TrimSpace(s), ".")
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if runes := []rune(s); len(runes) > 200 {
		s = string(runes[:200])
	}
	return s
}

// kindFolders are the top-level WebDAV folders TorBox files downloads under.
var kindFolders = map[string]string{
	torbox.KindTorrent: "torrents",
	torbox.KindUsenet:  "usenet",
	torbox.KindWebDL:   "webdl",
}

// WebDAVPaths lists candidate WebDAV paths for a download, most likely first.
// The Worker tries them in order.
func WebDAVPaths(itemName, fileName, kind string, flatten bool) []string {
	item := SanitizeSegment(itemName)
	file := SanitizeSegment(fileName)
	folder, ok := kindFolders[kind]
	if !ok {
		folder = "torrents"
	}

	var paths []string
	if flatten && file != "" {
		paths = append(paths, "/"+file)
	}
	if item != "" && file != "" {
		paths = append(paths, "/"+item+"/"+file, "/"+folder+"/"+item+"/"+file)
	}
	if file != "" {
		paths = append(paths, "/"+file, "/"+folder+"/"+file)
	}
	if item != "" && file == "" {
		paths = append(paths, "/"+item, "/"+folder+"/"+item)
	}

	seen := map[string]bool{}
	var unique []string
	for _, path := range paths {
		if path != "/" && !seen[path] {
			seen[path] = true
			unique = append(unique, path)
		}
	}
	return unique
}
