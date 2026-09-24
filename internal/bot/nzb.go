package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/crypt"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/nzbhydra"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

// idSeparators splits a list of IDs pasted with spaces, commas or newlines.
var idSeparators = regexp.MustCompile(`[ \n,]+`)

// maxIDsPerAdd bounds one /nzb against TorBox's create limits.
const maxIDsPerAdd = 10

// usenetLookupAttempts and usenetLookupInterval bound how long an add waits
// for Hydra's hand-off to show up in the TorBox usenet list.
const (
	usenetLookupAttempts = 6
	usenetLookupInterval = 2 * time.Second
)

// errInvalidNZBID marks a pasted token that is not a usable NZB ID.
//
// Bad base64, bytes that are not UTF-8, a missing "|", an unknown site: from
// the user's side these are all one thing, the ID is wrong, so they are listed
// together under INVALID NZB IDS.
var errInvalidNZBID = errors.New("invalid NZB ID")

// decodeNZBID turns an NZB ID from search results back into Hydra's result ID.
// A bare result ID is accepted too.
func decodeNZBID(token string) (string, error) {
	if nzbhydra.ValidResultID(token) {
		return token, nil
	}
	plaintext, err := crypt.Decrypt(token)
	if err != nil {
		return "", fmt.Errorf("%w: could not decode (%v)", errInvalidNZBID, err)
	}
	resultID, site, found := strings.Cut(plaintext, "|")
	if !found || strings.Contains(site, "|") {
		return "", fmt.Errorf("%w: decoded text is not '<id>|<site>'", errInvalidNZBID)
	}
	if site != hydraSite || !nzbhydra.ValidResultID(resultID) {
		return "", fmt.Errorf("%w: unknown site %q or bad id", errInvalidNZBID, site)
	}
	return resultID, nil
}

// nzbAddedText confirms one or more NZBs added from search results.
func nzbAddedText(names []string, userID int64, firstName string) string {
	return header("NZB ADDED") + "\n\n" + codeBlock(strings.Join(names, "\n\n")) + "\n\nby " + mention(userID, firstName)
}

// handleNZB adds an NZB file, replied to or captioned, or search results by
// the NZB IDs shown under them.
func (r *request) handleNZB(ctx context.Context) {
	b := r.bot
	document, filename, err := r.document(ctx)
	if err != nil {
		r.failAdd(ctx, "nzb", err)
		return
	}
	if document != nil && isNZBFile(filename) {
		r.addNZBFile(ctx, document, filename)
		return
	}

	var tokens []string
	for _, token := range idSeparators.Split(r.payload, -1) {
		if strings.TrimSpace(token) != "" {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		r.replyLogged(ctx, header("ADD NZB")+"\n\n"+
			"<code>/nzb [ID-1] [ID-2]...</code>\n\n"+
			"Use the NZB IDs from <code>/nzbsearch</code>, or reply to a <code>.nzb</code> file with <code>/nzb</code>, "+
			"or use <code>/nzb</code> as its caption.")
		return
	}
	if len(tokens) > maxIDsPerAdd {
		r.replyLogged(ctx, fmt.Sprintf("Add at most %d NZB IDs at a time.", maxIDsPerAdd))
		return
	}
	if b.hydra == nil {
		r.replyLogged(ctx, noIndexer)
		return
	}

	var added, failed []string
	// running is set once any added NZB still needs fetching; one that could
	// not be identified counts, since it may well be.
	running := false
	func() {
		done := r.adding(ctx, "Adding NZB")
		defer done()
		for _, token := range tokens {
			if ctx.Err() != nil {
				failed = append(failed, token)
				continue
			}
			resultID, err := decodeNZBID(token)
			if err != nil {
				// A mistyped or truncated ID is user input, not a fault, so
				// it joins the consolidated reply and is logged as a one-liner.
				b.log.Info("Invalid NZB ID", "token", token, "reason", err)
				failed = append(failed, token)
				continue
			}

			title := b.knownTitle(r.userID(), resultID)
			link, err := r.addResult(ctx, resultID, title)
			if err != nil {
				b.log.Warn("Error while adding", "id", resultID, "err", err)
				failed = append(failed, token)
				continue
			}

			name := title
			if name == "" {
				name = resultID
			}
			torboxID := "unknown"
			if link != nil {
				torboxID = itoa(link.ID)
				if link.Name != "" {
					name = link.Name
				}
				b.channel.enqueue(r.userID(), link)
			}
			if link == nil || (!running && b.stillRunning(ctx, link)) {
				running = true
			}
			b.log.Info("NZB added", "user_id", r.userID(), "id", resultID, "torbox_id", torboxID)
			added = append(added, name)
		}
	}()

	if len(added) > 0 {
		r.replyLogged(ctx, nzbAddedText(added, r.userID(), r.firstName()))
	}
	if len(failed) > 0 {
		r.replyLogged(ctx, header("INVALID NZB IDS")+"\n\n"+codeBlock(strings.Join(failed, "\n")))
	}
	if running && ctx.Err() == nil {
		r.runStatus(ctx, true)
	}
}

// addResult sends a result to TorBox through NZBHydra's downloader, then finds
// the download it became. It returns a nil link when Hydra accepted the result
// but the TorBox download could not be identified yet: it is still added, and
// only /dl and the channel post need the ID.
func (r *request) addResult(ctx context.Context, resultID, title string) (*torbox.Link, error) {
	b := r.bot
	// Several IDs in one /nzb trip the per-user cooldown; each waits it
	// out once rather than failing.
	if wait := b.cooldowns.take(r.userID()); wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait + 100*time.Millisecond):
		}
		if err := r.takeCooldown(); err != nil {
			return nil, err
		}
	}
	// Hydra's hand-off still counts against TorBox's usenet create limit.
	if err := b.torbox.CheckCreate(torbox.KindUsenet); err != nil {
		return nil, err
	}

	known, snapshotErr := b.usenetIDs(ctx)
	if snapshotErr != nil {
		b.log.Warn("Could not snapshot the usenet list before adding", "err", snapshotErr)
	}

	if err := b.hydra.Add(ctx, resultID, b.cfg.NZBHydraDownloaderName); err != nil {
		return nil, err
	}
	b.torbox.RecordCreate(torbox.KindUsenet)

	if snapshotErr != nil {
		return nil, nil
	}
	return b.findNewUsenet(ctx, title, known), nil
}

func (b *Bot) usenetIDs(ctx context.Context) (map[int64]bool, error) {
	items, err := b.torbox.ListAll(ctx, torbox.KindUsenet)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	return ids, nil
}

// findNewUsenet finds the usenet download NZBHydra just created.
//
// Only downloads absent from known count, so an older copy of the same release
// is never mistaken for the new one. A name match wins; a single unmatched new
// download is accepted on the last attempt, since TorBox may rename it.
func (b *Bot) findNewUsenet(ctx context.Context, title string, known map[int64]bool) *torbox.Link {
	wanted := nameKey(title)
	for attempt := 0; attempt < usenetLookupAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(usenetLookupInterval):
			}
		}
		items, err := b.torbox.ListAll(ctx, torbox.KindUsenet)
		if err != nil {
			b.log.Info("Usenet lookup failed", "attempt", attempt+1, "err", err)
			continue
		}

		var fresh []torbox.Item
		var pick *torbox.Item
		for _, item := range items {
			if known[item.ID] {
				continue
			}
			fresh = append(fresh, item)
			if wanted != "" && nameKey(item.Name) == wanted && (pick == nil || item.ID > pick.ID) {
				picked := item
				pick = &picked
			}
		}
		// Last resort only once the name had every chance to appear.
		if pick == nil && attempt == usenetLookupAttempts-1 && len(fresh) == 1 {
			pick = &fresh[0]
		}
		if pick != nil {
			return &torbox.Link{Kind: torbox.KindUsenet, ID: pick.ID, Name: pick.Name}
		}
	}
	return nil
}

var nameNoise = regexp.MustCompile(`[^a-z0-9]+`)

// nameKey compares release names ignoring case, separators and a .nzb suffix.
func nameKey(name string) string {
	base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".nzb")
	return nameNoise.ReplaceAllString(base, "")
}
