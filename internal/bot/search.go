package bot

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/message/markup"
	"github.com/gotd/td/tg"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/crypt"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/nzbhydra"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/telegraph"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/util"
)

const (
	maxSortFlag     = "--mx"
	minSortFlag     = "--mn"
	maxSessions     = 100
	telegraphTitle  = "NZB Search Results"
	telegraphAuthor = "USENET"
	// hydraSite is the site half of an NZB ID. The NZBGet bot uses the same
	// one, so IDs copied from either bot's results work with /nzb here.
	hydraSite = "nzbhydra"
)

// maxResults caps how many hits a search keeps, to bound the message and the
// Telegraph page. The cap is applied after sorting, never before: trimming
// first would make "largest" mean "largest among the newest 100".
const maxResults = 100

// redactedMessage replaces a result list once it is hidden, manually or on the
// redaction timer. It is a notice about the message, not content from it, so
// it is plain text; only indexer-supplied names stay monospace.
const redactedMessage = "RESULTS REDACTED"

// noIndexer is the reply to a search or add when NZBHydra is not configured.
const noIndexer = "No indexer configured, set NZBHYDRA_URL"

// searchSession is one rendered set of results, kept so its pagination buttons
// keep working after the message is sent.
type searchSession struct {
	results []nzbhydra.Result
	// found is the hit count before the cap, so a trimmed list can say so.
	found    int
	query    string
	sortMode string
	userID   int64

	// The Telegraph page is published after the results are already on screen,
	// so these are written by that goroutine while the pagination callbacks
	// read them. mu covers everything below it.
	mu            sync.Mutex
	page          int
	telegraph     *telegraph.Client
	telegraphURL  string
	telegraphPath string
	redacted      bool
	// target and msgID locate the results message once it has been sent, so
	// a session dropped for any reason can still be blanked on screen.
	target tg.InputPeerClass
	msgID  int

	// cancelRedact stops the auto-redact timer. It is guarded by Bot.mu, not
	// by mu above, because it is set and cleared alongside the session map.
	cancelRedact context.CancelFunc
}

// setMessage records where the results were posted.
func (s *searchSession) setMessage(target tg.InputPeerClass, msgID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = target
	s.msgID = msgID
}

// message returns where the results were posted, if they were.
func (s *searchSession) message() (tg.InputPeerClass, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target, s.msgID
}

// setTelegraph records a freshly published page and reports whether it is
// still wanted. A session redacted while the page was being built keeps its
// URL off the keyboard, and the caller blanks the page instead.
func (s *searchSession) setTelegraph(client *telegraph.Client, url, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redacted {
		return false
	}
	s.telegraph, s.telegraphURL, s.telegraphPath = client, url, path
	return true
}

// telegraphURLValue is the published page, or "" while it is still being made.
func (s *searchSession) telegraphURLValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.telegraphURL
}

// markRedacted closes the session to further publishing and hands back the
// page to blank, if one exists yet.
func (s *searchSession) markRedacted() (*telegraph.Client, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redacted = true
	return s.telegraph, s.telegraphPath
}

// setPage and currentPage track which page is on screen, so the edit that adds
// the Telegraph button does not drag a user who has already paged on back to
// the first page.
func (s *searchSession) setPage(page int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.page = page
}

func (s *searchSession) currentPage() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.page
}

// handleSearch queries NZBHydra and posts the first page of results.
func (r *request) handleSearch(ctx context.Context) {
	b := r.bot
	if r.payload == "" {
		r.replyLogged(ctx, "Provide a query")
		return
	}
	query, sortMode, parseErr := parseSearchArgs(r.payload)
	if parseErr != "" {
		r.replyLogged(ctx, escape(parseErr))
		return
	}
	if strings.TrimSpace(query) == "" {
		r.replyLogged(ctx, "Provide a query")
		return
	}
	if b.hydra == nil {
		r.replyLogged(ctx, noIndexer)
		return
	}

	sortText := ""
	switch sortMode {
	case "max":
		sortText = " (sorted by size: largest first)"
	case "min":
		sortText = " (sorted by size: smallest first)"
	}
	noticeID, err := r.reply(ctx, "Searching"+sortText)
	if err != nil {
		b.log.Warn("Failed to post status message", "err", err)
		return
	}

	start := time.Now()
	results, err := b.hydra.Search(ctx, query)
	if err != nil {
		b.log.Error("Error occurred while searching", "query", query, "err", err)
		r.editLogged(ctx, noticeID, b.errorText(err))
		return
	}
	found := len(results)
	results = sortResults(results, sortMode)
	b.log.Info("Search complete", "query", query, "sort", sortMode,
		"results", len(results), "found", found, "took", time.Since(start).Round(time.Millisecond))
	if len(results) == 0 {
		r.editLogged(ctx, noticeID, "No results")
		return
	}

	session := &searchSession{
		results:  results,
		found:    found,
		query:    query,
		sortMode: sortMode,
		userID:   r.userID(),
	}
	token, err := newToken(8)
	if err != nil {
		b.log.Error("Could not create search token", "err", err)
		r.editLogged(ctx, noticeID, errUnexpected)
		return
	}
	b.storeSearch(token, session)

	text, totalPages, page := b.searchPageText(session, 0)
	keyboard := b.searchMarkup(token, session, page, totalPages)

	r.deleteLogged(ctx, noticeID)
	sentID, err := r.replyMarkup(ctx, text, keyboard)
	if err != nil {
		b.log.Warn("Failed to show search results", "err", err)
		return
	}
	target, err := r.peer()
	if err != nil {
		return
	}
	session.setMessage(target, sentID)
	b.scheduleAutoRedact(token)

	// The Telegraph mirror is published only now. Building it first meant every
	// search waited on two more round trips before a single result appeared,
	// for a button most searches never use. The keyboard is edited once the
	// page exists, on whichever page the reader has since turned to.
	go func() {
		defer b.recoverPanic("telegraph publish")

		if !b.publishTelegraph(b.runCtx, session) {
			return
		}
		if b.getSearch(token) == nil {
			return
		}
		text, totalPages, safePage := b.searchPageText(session, session.currentPage())
		if err := b.edit(b.runCtx, target, sentID, text, b.searchMarkup(token, session, safePage, totalPages)); err != nil {
			b.log.Debug("Could not attach the Telegraph button", "err", err)
		}
	}()
}

// sortResults orders by size when asked, then caps the list.
func sortResults(results []nzbhydra.Result, sortMode string) []nzbhydra.Result {
	switch sortMode {
	case "max":
		sort.SliceStable(results, func(a, b int) bool { return results[a].Size > results[b].Size })
	case "min":
		sort.SliceStable(results, func(a, b int) bool { return results[a].Size < results[b].Size })
	}
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results
}

// nzbID is the ID shown under a search result for /nzb.
func nzbID(resultID string) string {
	token, err := crypt.Encrypt(resultID + "|" + hydraSite)
	if err != nil {
		return resultID
	}
	return token
}

func resultAge(result nzbhydra.Result) string {
	if result.Published.IsZero() {
		return "Not Found"
	}
	return util.Age(result.Published)
}

// publishTelegraph mirrors the full result list to a Telegraph page and
// reports whether the page is live and wanted. A failure only costs the URL
// button, so it is logged rather than surfaced.
func (b *Bot) publishTelegraph(ctx context.Context, session *searchSession) bool {
	client, err := b.telegraphAccount(ctx)
	if err != nil {
		b.log.Warn("Telegraph failed", "err", err)
		return false
	}

	content := make([]telegraph.Node, 0, len(session.results))
	for _, result := range session.results {
		content = append(content,
			telegraph.Tag("p",
				telegraph.Tag("code", telegraph.Text(result.Title)),
				telegraph.Text(fmt.Sprintf(" - %s  (Age: %s)", util.ConvertSize(result.Size), resultAge(result))),
				telegraph.Tag("br"),
				telegraph.Text("NZB ID: "),
				telegraph.Tag("code", telegraph.Text(nzbID(result.ID))),
			),
		)
	}

	page, err := client.CreatePage(ctx, telegraphTitle, telegraphAuthor, content)
	if err != nil {
		b.log.Warn("Telegraph failed", "err", err)
		return false
	}

	if !session.setTelegraph(client, page.URL, page.Path) {
		// Closed or auto-redacted while the page was being built. It lists every
		// result and its NZB ID, so it cannot be left standing.
		b.blankPage(ctx, client, page.Path)
		return false
	}
	return true
}

// telegraphAccount returns the account every search publishes under, creating
// it on first use. Making one per search cost a round trip before any result
// could be shown, and left a new throwaway account behind each time.
func (b *Bot) telegraphAccount(ctx context.Context) (*telegraph.Client, error) {
	b.telegraphMu.Lock()
	defer b.telegraphMu.Unlock()

	if b.telegraph != nil {
		return b.telegraph, nil
	}
	client, err := telegraph.NewAccount(ctx, "NZBSearch")
	if err != nil {
		return nil, err
	}
	b.telegraph = client
	return client, nil
}

// redactTelegraph blanks the page that accompanies a search. It lists every
// result and its NZB ID, so wiping only the Telegram message would leave the
// whole thing public.
func (b *Bot) redactTelegraph(ctx context.Context, session *searchSession) {
	client, path := session.markRedacted()
	b.blankPage(ctx, client, path)
}

// blankPage replaces a published page with a placeholder. The page is never
// deleted, because Telegraph has no delete: blanking it is the only redaction
// there is.
func (b *Bot) blankPage(ctx context.Context, client *telegraph.Client, path string) {
	if client == nil || path == "" {
		return
	}
	content := []telegraph.Node{telegraph.Tag("p", telegraph.Text("REDACTED"))}
	if err := client.EditPage(ctx, path, telegraphTitle, telegraphAuthor, content); err != nil {
		b.log.Warn("Telegraph redact failed", "err", err)
	}
}

// scheduleAutoRedact wipes the results after NZBSEARCH_AUTOREDACT, if set.
func (b *Bot) scheduleAutoRedact(token string) {
	if b.cfg.SearchRedact <= 0 {
		return
	}

	ctx, cancel := context.WithCancel(b.runCtx)

	b.mu.Lock()
	session, ok := b.searches[token]
	if ok {
		if session.cancelRedact != nil {
			session.cancelRedact()
		}
		session.cancelRedact = cancel
	}
	b.mu.Unlock()

	if !ok {
		cancel()
		return
	}

	// Deliberately not tracked by the shutdown WaitGroup: nothing should wait
	// minutes for this timer when the bot is stopping.
	go func() {
		defer cancel()
		defer b.recoverPanic("auto-redact")

		select {
		case <-ctx.Done():
			return
		case <-time.After(b.cfg.SearchRedact):
		}

		if expired := b.takeSearch(token); expired != nil {
			b.redactSession(b.runCtx, expired)
		}
	}()
}

// redactSession blanks everything a dropped session left in public: the
// Telegraph page and the results message itself.
func (b *Bot) redactSession(ctx context.Context, session *searchSession) {
	b.redactTelegraph(ctx, session)
	target, msgID := session.message()
	if target == nil {
		return
	}
	if err := b.edit(ctx, target, msgID, redactedMessage, nil); err != nil && !isNotModified(err) {
		b.log.Debug("Could not mark results redacted", "err", err)
	}
}

func (b *Bot) storeSearch(token string, session *searchSession) {
	b.mu.Lock()
	b.searches[token] = session
	b.searchOrder = append(b.searchOrder, token)

	// Sessions pushed out by the cap are redacted like any other: their
	// message and page list every result and NZB ID, and with the timer
	// cancelled nothing else would ever hide them.
	var evicted []*searchSession
	for len(b.searchOrder) > maxSessions {
		oldest := b.searchOrder[0]
		b.searchOrder = b.searchOrder[1:]
		if old, ok := b.searches[oldest]; ok {
			if old.cancelRedact != nil {
				old.cancelRedact()
				old.cancelRedact = nil
			}
			delete(b.searches, oldest)
			evicted = append(evicted, old)
		}
	}
	b.mu.Unlock()

	for _, old := range evicted {
		go func() {
			defer b.recoverPanic("evict search")
			b.redactSession(b.runCtx, old)
		}()
	}
}

func (b *Bot) getSearch(token string) *searchSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.searches[token]
}

// takeSearch removes and returns a session, so a close and an auto-redact
// firing together cannot both act on it. The auto-redact timer is stopped on
// the way out, under the same lock that set it.
func (b *Bot) takeSearch(token string) *searchSession {
	b.mu.Lock()
	defer b.mu.Unlock()

	session, ok := b.searches[token]
	if !ok {
		return nil
	}
	if session.cancelRedact != nil {
		session.cancelRedact()
		session.cancelRedact = nil
	}
	delete(b.searches, token)
	for i, candidate := range b.searchOrder {
		if candidate == token {
			b.searchOrder = append(b.searchOrder[:i], b.searchOrder[i+1:]...)
			break
		}
	}
	return session
}

// knownTitle is the title of a result ID from one of the user's live searches.
// TorBox gets the NZB from Hydra, so the title is how the new download is
// told apart from everything else in the usenet list.
func (b *Bot) knownTitle(userID int64, resultID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.searchOrder) - 1; i >= 0; i-- {
		session := b.searches[b.searchOrder[i]]
		if session == nil || session.userID != userID {
			continue
		}
		for _, result := range session.results {
			if result.ID == resultID {
				return result.Title
			}
		}
	}
	return ""
}

func (b *Bot) searchPageText(session *searchSession, page int) (string, int, int) {
	perPage := b.cfg.SearchResultsPerPage
	total := len(session.results)
	totalPages := max(1, (total+perPage-1)/perPage)
	page = min(max(page, 0), totalPages-1)

	start := page * perPage
	end := min(start+perPage, total)

	entries := make([]string, 0, end-start)
	for _, result := range session.results[start:end] {
		entries = append(entries, fmt.Sprintf(
			"<code>%s</code> - %s  (Age: %s)\nNZB ID: <code>%s</code>",
			escape(result.Title), util.ConvertSize(result.Size), resultAge(result), nzbID(result.ID)))
	}

	sortInfo := ""
	switch session.sortMode {
	case "max":
		sortInfo = " (sorted by size: max to min)"
	case "min":
		sortInfo = " (sorted by size: min to max)"
	}

	// Say when the list is only part of what Hydra returned, rather than
	// presenting a capped set as if it were everything.
	shown := fmt.Sprintf("Total: %d", total)
	if session.found > total {
		shown = fmt.Sprintf("Showing %d of %d", total, session.found)
	}

	head := header("NZB SEARCH RESULTS" + sortInfo)
	meta := fmt.Sprintf("<b>Query:</b> <code>%s</code>\n<b>Page %d/%d | %s</b>",
		escape(session.query), page+1, totalPages, shown)
	return head + "\n" + meta + "\n\n" + strings.Join(entries, "\n\n"), totalPages, page
}

func (b *Bot) searchMarkup(token string, session *searchSession, page, totalPages int) tg.ReplyMarkupClass {
	var nav []tg.KeyboardButtonClass
	if page > 0 {
		nav = append(nav, markup.Callback("PREV", []byte(fmt.Sprintf("srch:%s:%d", token, page-1))))
	}
	nav = append(nav, markup.Callback(fmt.Sprintf("%d/%d", page+1, totalPages), []byte("srchnoop")))
	if page < totalPages-1 {
		nav = append(nav, markup.Callback("NEXT", []byte(fmt.Sprintf("srch:%s:%d", token, page+1))))
	}

	rows := []tg.KeyboardButtonRow{markup.Row(nav...)}
	if url := session.telegraphURLValue(); url != "" {
		rows = append(rows, markup.Row(markup.URL("URL", url)))
	}
	rows = append(rows, markup.Row(markup.Callback("CLOSE", []byte("srchc:"+token))))
	return markup.InlineKeyboard(rows...)
}

// onSearchCallback drives the pagination and close buttons.
func (b *Bot) onSearchCallback(cb *callback) {
	data := cb.data

	if data == "srchnoop" {
		b.answerCallback(cb.ctx, cb.queryID, "", false)
		return
	}

	if token, ok := strings.CutPrefix(data, "srchc:"); ok {
		session := b.getSearch(token)
		if session == nil {
			b.answerCallback(cb.ctx, cb.queryID, "Session expired", true)
			return
		}
		if cb.userID != session.userID {
			b.answerCallback(cb.ctx, cb.queryID, "Only requester can close this.", true)
			return
		}
		if closed := b.takeSearch(token); closed != nil {
			b.redactTelegraph(cb.ctx, closed)
		}
		b.editLogged(cb.ctx, cb.peer, cb.msgID, redactedMessage)
		b.answerCallback(cb.ctx, cb.queryID, "Closed", false)
		return
	}

	rest, ok := strings.CutPrefix(data, "srch:")
	if !ok {
		b.answerCallback(cb.ctx, cb.queryID, "", false)
		return
	}
	token, pageText, found := strings.Cut(rest, ":")
	if !found {
		b.answerCallback(cb.ctx, cb.queryID, "", false)
		return
	}

	session := b.getSearch(token)
	if session == nil {
		b.answerCallback(cb.ctx, cb.queryID, "Session expired", true)
		return
	}
	if cb.userID != session.userID {
		b.answerCallback(cb.ctx, cb.queryID, "Only requester can change pages.", true)
		return
	}
	page, err := strconv.Atoi(pageText)
	if err != nil {
		b.answerCallback(cb.ctx, cb.queryID, "", false)
		return
	}

	text, totalPages, safePage := b.searchPageText(session, page)
	session.setPage(safePage)
	keyboard := b.searchMarkup(token, session, safePage, totalPages)
	if err := b.edit(cb.ctx, cb.peer, cb.msgID, text, keyboard); err != nil && !isNotModified(err) {
		b.log.Debug("Could not turn search page", "err", err)
	}
	b.answerCallback(cb.ctx, cb.queryID, "", false)
}

// parseSearchArgs splits the query from an optional sort flag. A flag counts
// only as the last token, so a query may still contain the characters.
func parseSearchArgs(text string) (query, sortMode, parseError string) {
	args := searchTerms(text)
	if len(args) == 0 {
		return text, "", ""
	}

	maxCount, minCount := 0, 0
	for _, arg := range args {
		switch arg {
		case maxSortFlag:
			maxCount++
		case minSortFlag:
			minCount++
		}
	}
	if maxCount > 1 || minCount > 1 {
		return "", "", "Use --mx/--mn only once, at the end."
	}
	if maxCount > 0 && minCount > 0 {
		return "", "", "Use either --mx or --mn, not both."
	}

	if maxCount > 0 || minCount > 0 {
		last := args[len(args)-1]
		if last != maxSortFlag && last != minSortFlag {
			return "", "", "Use --mx or --mn only at the end. Example: /nzbsearch ubuntu --mn"
		}
		if last == maxSortFlag {
			sortMode = "max"
		} else {
			sortMode = "min"
		}
		args = args[:len(args)-1]
	}
	return strings.Join(args, " "), sortMode, ""
}

// searchTerms splits a query into words. A search query is prose, not a shell
// command, so nothing here can fail and no character is special: "Marvel's
// Spider-Man 2" has to work. Surrounding quotes are trimmed, for anyone who
// quotes a phrase out of habit, but an internal apostrophe is kept: indexers
// match titles literally, and "Marvels" does not find "Marvel's".
func searchTerms(text string) []string {
	var terms []string
	for _, field := range strings.Fields(text) {
		if field = strings.Trim(field, `'"`); field != "" {
			terms = append(terms, field)
		}
	}
	return terms
}
