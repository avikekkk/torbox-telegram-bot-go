package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/nzbhydra"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

// maxUploadBytes caps a .torrent or .nzb pulled from Telegram into memory.
const maxUploadBytes = 20 << 20

// createCooldown is the least time between two adds by one user, so a burst
// of commands cannot stampede TorBox's create limits.
const createCooldown = 2500 * time.Millisecond

var (
	magnetPattern = regexp.MustCompile(`(?i)magnet:\?xt=urn:btih:([a-f0-9]{40})`)
	hashPattern   = regexp.MustCompile(`(?i)^[a-f0-9]{40}$`)
)

// Error statuses shown to users.
const (
	errUnexpected  = "Unexpected error"
	errInterrupted = "Interrupted by a bot restart, please try again"
)

func header(text string) string { return "<u><b>" + text + "</b></u>" }

func codeBlock(text string) string { return "<code>" + html.EscapeString(text) + "</code>" }

// mention renders an inline link to a user profile.
func mention(userID int64, firstName string) string {
	if firstName == "" {
		firstName = "Unknown"
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, userID, html.EscapeString(firstName))
}

// addedTitles are the headers of the confirmation each kind of add sends.
var addedTitles = map[string]string{
	torbox.KindTorrent: "TORRENT ADDED",
	torbox.KindUsenet:  "NZB ADDED",
	torbox.KindWebDL:   "WEB ADDED",
}

// addedText confirms an add. The name links to the download when a Worker
// link could be made for it already, as for a cached torrent.
func addedText(kind, name string, userID int64, firstName, link string) string {
	if name == "" {
		name = "Unknown"
	}
	displayed := codeBlock(name)
	if link != "" {
		displayed = `<a href="` + html.EscapeString(link) + `">` + displayed + "</a>"
	}
	return header(addedTitles[kind]) + "\n\n" + displayed + "\n\nby " + mention(userID, firstName)
}

// errorText is the reply for a failed command: a TorBox or Hydra message as
// is, since those are written for users, with a hint where one helps.
func (b *Bot) errorText(err error) string {
	var tbErr *torbox.Error
	var hydraErr *nzbhydra.Error
	var userErr *userError
	switch {
	case b.interrupted() || errors.Is(err, context.Canceled):
		return errInterrupted
	case errors.As(err, &tbErr), errors.As(err, &userErr):
		return torboxErrorText(err.Error())
	case errors.As(err, &hydraErr):
		return html.EscapeString(err.Error())
	}
	return errUnexpected
}

// torboxErrorText adds a recovery hint to a TorBox message when one applies.
func torboxErrorText(raw string) string {
	lower := strings.ToLower(raw)
	var tips []string
	if containsAny(lower, "not ready", "not cached", "still downloading", "no files",
		"download is not", "not completed", "processing") {
		tips = append(tips, "Item may still be downloading — wait, then /dl again.")
	}
	if strings.Contains(raw, "400") || strings.Contains(lower, "invalid") {
		tips = append(tips, "Check the download ID: "+codeBlock("/dl 42"))
	}
	if containsAny(lower, "api key", "auth") || strings.Contains(raw, "401") || strings.Contains(raw, "403") {
		tips = append(tips, "Ask the bot owner to check the TorBox API key.")
	}

	if runes := []rune(raw); len(runes) > 600 {
		raw = string(runes[:599]) + "…"
	}
	text := html.EscapeString(raw)
	if len(tips) > 0 {
		text += "\n\n" + strings.Join(tips, "\n")
	}
	return text
}

// userError is a problem with what the user sent, worded for them.
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// cooldowns spaces out adds per user.
type cooldowns struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

func newCooldowns() *cooldowns { return &cooldowns{last: map[int64]time.Time{}} }

// take claims the user's next add, or reports how long until one is allowed.
func (c *cooldowns) take(userID int64) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := createCooldown - time.Since(c.last[userID]); wait > 0 {
		return wait
	}
	c.last[userID] = time.Now()
	return 0
}

// takeCooldown fails an add that follows the user's previous one too closely.
func (r *request) takeCooldown() error {
	if wait := r.bot.cooldowns.take(r.userID()); wait > 0 {
		return &userError{fmt.Sprintf("Slow down — try again in %.1fs (create).", wait.Seconds())}
	}
	return nil
}

// documentOf returns the file a message carries, if any.
func documentOf(msg *tg.Message) (*tg.Document, string, bool) {
	if msg == nil {
		return nil, "", false
	}
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, "", false
	}
	document, ok := media.Document.(*tg.Document)
	if !ok {
		return nil, "", false
	}
	var filename string
	for _, attribute := range document.Attributes {
		if name, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			filename = strings.TrimSpace(name.FileName)
			break
		}
	}
	// Telegram keeps whatever the sender named the file, so "x.TORRENT" and a
	// torrent sent with only its MIME type must both count.
	mime := strings.ToLower(document.MimeType)
	if filename == "" && strings.Contains(mime, "bittorrent") {
		filename = "file.torrent"
	}
	if filename == "" && strings.Contains(mime, "nzb") {
		filename = "file.nzb"
	}
	return document, filename, true
}

func isTorrentFile(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".torrent") }

func isNZBFile(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".nzb") }

// document finds the file a command is about: attached to the command itself
// (a caption), or on the message it replied to.
func (r *request) document(ctx context.Context) (*tg.Document, string, error) {
	if document, name, ok := documentOf(r.msg); ok {
		return document, name, nil
	}
	replied, err := r.repliedMessage(ctx)
	if err != nil {
		return nil, "", err
	}
	document, name, _ := documentOf(replied)
	return document, name, nil
}

// repliedText is the text of the message this command replied to, so a
// magnet or URL can be added by replying to it.
func (r *request) repliedText(ctx context.Context) string {
	replied, err := r.repliedMessage(ctx)
	if err != nil {
		r.bot.log.Warn("Failed to fetch replied message", "err", err)
		return ""
	}
	if replied == nil {
		return ""
	}
	return strings.TrimSpace(replied.Message)
}

// download pulls a Telegram file into memory.
func (r *request) download(ctx context.Context, document *tg.Document) ([]byte, error) {
	tooLarge := func(size int64) error {
		return &userError{fmt.Sprintf("File is too large (%d MB). Max supported here is %d MB.",
			size>>20, maxUploadBytes>>20)}
	}
	if document.Size > maxUploadBytes {
		return nil, tooLarge(document.Size)
	}
	var buf bytes.Buffer
	// Empty thumb size: the file itself, not a preview.
	_, err := downloader.NewDownloader().Download(r.bot.api, document.AsInputDocumentFileLocation("")).Stream(ctx, &buf)
	if err != nil {
		return nil, fmt.Errorf("download from Telegram: %w", err)
	}
	if buf.Len() > maxUploadBytes {
		return nil, tooLarge(int64(buf.Len()))
	}
	return buf.Bytes(), nil
}

// adding keeps a plain-text notice on screen while a slow add runs, and
// removes it once the outcome is posted.
func (r *request) adding(ctx context.Context, text string) func() {
	noticeID, err := r.reply(ctx, text)
	if err != nil {
		r.bot.log.Warn("Failed to post notice", "err", err)
		return func() {}
	}
	return func() { r.deleteLogged(ctx, noticeID) }
}

// finishAdd confirms an add, queues the download for the channel, and opens a
// live /status so the new download can be watched, as the NZBGet bot does. A
// cached download is already done, so it gets no status.
func (r *request) finishAdd(ctx context.Context, link *torbox.Link) {
	r.bot.log.Info("Download added", "kind", link.Kind, "id", link.ID, "name", link.Name,
		"cached", link.URL != "", "user_id", r.userID())
	// A cached download is ready now, so its name links to the file page.
	public := ""
	if link.URL != "" {
		public = r.bot.links.Page(link.Kind, link.ID, link.Name)
	}
	r.replyLogged(ctx, addedText(link.Kind, link.Name, r.userID(), r.firstName(), public))
	r.bot.channel.enqueue(r.userID(), link)
	if ctx.Err() == nil && r.bot.stillRunning(ctx, link) {
		r.runStatus(ctx, true)
	}
}

// stillRunning reports whether a download just added is still being fetched.
// A link TorBox handed over at once means it was cached. When TorBox cannot be
// asked, the status is shown anyway: it costs one message at most.
func (b *Bot) stillRunning(ctx context.Context, link *torbox.Link) bool {
	if link.URL != "" {
		return false
	}
	item, err := b.torbox.Get(ctx, link.Kind, link.ID)
	if err != nil {
		return true
	}
	return item.IsActive()
}

// failAdd reports a failed add.
func (r *request) failAdd(ctx context.Context, what string, err error) {
	var tbErr *torbox.Error
	var userErr *userError
	if errors.As(err, &tbErr) || errors.As(err, &userErr) {
		r.bot.log.Warn("Add failed", "what", what, "reason", err)
	} else {
		r.bot.log.Error("Add failed", "what", what, "err", err)
	}
	r.replyLogged(ctx, r.bot.errorText(err))
}

func (r *request) handleTorrent(ctx context.Context) {
	document, filename, err := r.document(ctx)
	if err != nil {
		r.failAdd(ctx, "torrent", err)
		return
	}
	// Only a torrent file is an upload; a reply to anything else may still
	// carry a magnet in the command.
	if document != nil && isTorrentFile(filename) {
		done := r.adding(ctx, "Adding torrent file")
		defer done()

		content, err := r.download(ctx, document)
		if err != nil {
			r.failAdd(ctx, "torrent file", err)
			return
		}
		if err := r.takeCooldown(); err != nil {
			r.failAdd(ctx, "torrent file", err)
			return
		}
		link, err := r.bot.torbox.AddTorrent(ctx, "", strings.TrimSuffix(filename, path.Ext(filename)), content, filename)
		if err != nil {
			r.failAdd(ctx, "torrent file", err)
			return
		}
		r.finishAdd(ctx, link)
		return
	}

	raw := r.payload
	if raw == "" {
		raw = r.repliedText(ctx)
	}
	if raw == "" {
		if document != nil {
			r.replyLogged(ctx, "Provide a <code>.torrent</code> file or use <code>/torrent [magnet or hash]</code>.")
			return
		}
		r.replyLogged(ctx, header("ADD TORRENT")+"\n\n"+
			"<code>/torrent [magnet or hash]</code>\n\n"+
			"Or reply to a <code>.torrent</code> file with <code>/torrent</code>, or use it as the file caption.")
		return
	}

	magnet, _ := parseMagnet(raw)
	if magnet == "" {
		r.replyLogged(ctx, "Provide a valid magnet or info hash")
		return
	}

	done := r.adding(ctx, "Adding torrent")
	defer done()
	if err := r.takeCooldown(); err != nil {
		r.failAdd(ctx, "torrent", err)
		return
	}
	link, err := r.bot.torbox.AddTorrent(ctx, magnet, magnetName(magnet), nil, "")
	if err != nil {
		r.failAdd(ctx, "torrent", err)
		return
	}
	r.finishAdd(ctx, link)
}

// parseMagnet accepts a magnet link or a bare 40-character info hash, and
// returns the magnet to add and its lower-case hash.
func parseMagnet(text string) (magnet, hash string) {
	text = strings.TrimSpace(text)
	if match := magnetPattern.FindStringSubmatch(text); match != nil {
		hash = strings.ToLower(match[1])
		if strings.HasPrefix(strings.ToLower(text), "magnet:") {
			return text, hash
		}
		return "magnet:?xt=urn:btih:" + hash, hash
	}
	if strings.HasPrefix(strings.ToLower(text), "magnet:") {
		// A base32 hash or a v2 magnet: TorBox can still read it.
		return text, ""
	}
	if hashPattern.MatchString(text) {
		hash = strings.ToLower(text)
		return "magnet:?xt=urn:btih:" + hash, hash
	}
	return "", ""
}

// magnetName is the optional dn display name inside a magnet link.
func magnetName(magnet string) string {
	_, rawQuery, found := strings.Cut(magnet, "?")
	if !found {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(values.Get("dn"))
}

// addNZBFile uploads an NZB document to TorBox.
func (r *request) addNZBFile(ctx context.Context, document *tg.Document, filename string) {
	done := r.adding(ctx, "Adding NZB file")
	defer done()

	content, err := r.download(ctx, document)
	if err != nil {
		r.failAdd(ctx, "nzb file", err)
		return
	}
	if err := r.takeCooldown(); err != nil {
		r.failAdd(ctx, "nzb file", err)
		return
	}
	link, err := r.bot.torbox.AddUsenet(ctx, content, filename, strings.TrimSuffix(filename, path.Ext(filename)))
	if err != nil {
		r.failAdd(ctx, "nzb file", err)
		return
	}
	r.finishAdd(ctx, link)
}

func (r *request) handleWeb(ctx context.Context) {
	raw := r.payload
	if raw == "" {
		raw = r.repliedText(ctx)
	}
	if raw == "" {
		r.replyLogged(ctx, header("ADD WEB DOWNLOAD")+"\n\n"+
			"<code>/web [url]</code>\n\n"+
			"Send a hoster or direct URL, or reply to a link with <code>/web</code>.")
		return
	}

	done := r.adding(ctx, "Adding web download")
	defer done()

	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		r.failAdd(ctx, "web", &userError{"Provide an http(s) URL for web download."})
		return
	}
	if err := r.takeCooldown(); err != nil {
		r.failAdd(ctx, "web", err)
		return
	}
	link, err := r.bot.torbox.AddWeb(ctx, raw)
	if err != nil {
		r.failAdd(ctx, "web", err)
		return
	}
	r.finishAdd(ctx, link)
}

// handleDocumentHint nudges a user who sent a bare .torrent or .nzb towards the
// command that adds it. Anyone not authorized hears nothing: this is not a
// command, and the bot answers only commands from strangers.
func (r *request) handleDocumentHint(ctx context.Context) {
	if !r.isAuthorized() {
		return
	}
	_, name, _ := documentOf(r.msg)
	if isNZBFile(name) {
		r.replyLogged(ctx, "NZB file received. Reply with <code>/nzb</code> to add it.")
		return
	}
	r.replyLogged(ctx, "Torrent file received. Reply with <code>/torrent</code> to add it.")
}
