package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/proxy"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

// kindAliases maps what /dl accepts before an ID to a TorBox library.
var kindAliases = map[string]string{
	"t": torbox.KindTorrent, "tor": torbox.KindTorrent, "torrent": torbox.KindTorrent,
	"u": torbox.KindUsenet, "usenet": torbox.KindUsenet, "n": torbox.KindUsenet, "nzb": torbox.KindUsenet,
	"w": torbox.KindWebDL, "web": torbox.KindWebDL, "webdl": torbox.KindWebDL,
}

// dlArgs is a parsed "/dl [kind] <id> [file_id]".
type dlArgs struct {
	kind string
	// explicit is false when the kind was defaulted, so /dl tries every
	// library.
	explicit bool
	id       int64
	fileID   *int64
}

// cleanIDToken strips what comes along when an ID is copied out of Telegram:
// backticks, quotes, a leading # and trailing punctuation.
func cleanIDToken(raw string) string {
	s := strings.Trim(strings.TrimSpace(raw), "`\"'")
	s = strings.TrimLeft(strings.TrimSpace(s), "#")
	return strings.TrimSpace(strings.TrimRight(s, ",;"))
}

func parseDLArgs(args []string) (dlArgs, error) {
	var rest []string
	for _, arg := range args {
		if token := cleanIDToken(arg); token != "" {
			rest = append(rest, token)
		}
	}
	parsed := dlArgs{kind: torbox.KindTorrent}
	if len(rest) > 0 {
		if kind, ok := kindAliases[strings.ToLower(rest[0])]; ok {
			parsed.kind, parsed.explicit = kind, true
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return dlArgs{}, errors.New("missing download id")
	}

	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return dlArgs{}, err
	}
	parsed.id = id

	if len(rest) >= 2 {
		// A file ID, bare or as "f <file_id>".
		fileToken := rest[1]
		if lower := strings.ToLower(fileToken); (lower == "f" || lower == "file") && len(rest) >= 3 {
			fileToken = rest[2]
		}
		fileID, err := strconv.ParseInt(fileToken, 10, 64)
		if err != nil {
			return dlArgs{}, err
		}
		parsed.fileID = &fileID
	}
	return parsed, nil
}

func (r *request) handleDownload(ctx context.Context) {
	if len(r.args) == 0 {
		r.replyLogged(ctx, header("DOWNLOAD LINK")+"\n\n"+
			codeBlock("/dl 42")+" — get the full download link\n\n"+
			"The download type is detected automatically.\n"+
			"Find the ID in /status or your TorBox dashboard.")
		return
	}
	args, err := parseDLArgs(r.args)
	if err != nil {
		r.replyLogged(ctx, "Invalid download ID.\n\nUsage: "+codeBlock("/dl 42"))
		return
	}

	// The whole download as one zip, unless a single file was asked for.
	zip := args.fileID == nil
	link, err := r.bot.requestLink(ctx, args, zip)
	if err != nil {
		r.bot.log.Warn("Download link failed", "id", args.id, "kind", args.kind, "reason", err)
		r.replyLogged(ctx, r.bot.errorText(err))
		return
	}
	r.bot.log.Info("Download link requested", "id", link.ID, "kind", link.Kind, "user_id", r.userID())
	r.sendDownload(ctx, link, zip, args.fileID)
}

// stillWorkingStates mean a zip failed because the download has not finished.
var stillWorkingStates = []string{"downloading", "queued", "pending", "meta", "waiting", "processing"}

// requestLink gets a CDN link for an existing download.
//
// TorBox answers a wrong library or an unfinished download with a bare 400, so
// this works around the common cases: a defaulted kind tries every library in
// turn, a zip that fails falls back to the first file, and a download still in
// progress is reported as such rather than as a bad ID.
func (b *Bot) requestLink(ctx context.Context, args dlArgs, zip bool) (*torbox.Link, error) {
	kinds := []string{args.kind}
	if !args.explicit {
		for _, kind := range torbox.Kinds {
			if kind != args.kind {
				kinds = append(kinds, kind)
			}
		}
	}
	fileID := args.fileID
	if zip {
		// TorBox gives zip precedence over file_id anyway.
		fileID = nil
	}

	var lastErr error
	for _, kind := range kinds {
		url, err := b.torbox.RequestLink(ctx, kind, args.id, fileID, zip)
		if err == nil {
			link := &torbox.Link{Kind: kind, ID: args.id, URL: url}
			// The name is only decoration, so a failed lookup costs nothing.
			if item, err := b.torbox.Get(ctx, kind, args.id); err == nil {
				link.Name = item.Name
			}
			return link, nil
		}
		lastErr = err

		status := torbox.StatusOf(err)
		switch {
		case errors.Is(err, context.Canceled):
			return nil, err
		case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusTooManyRequests:
			return nil, err
		case args.explicit && fileID != nil:
			return nil, err
		}

		if zip && (status == http.StatusBadRequest || status == 0) {
			item, getErr := b.torbox.Get(ctx, kind, args.id)
			if getErr == nil {
				state := strings.ToLower(item.State)
				if containsAny(state, stillWorkingStates...) {
					return nil, &torbox.Error{Status: status, Message: fmt.Sprintf(
						"Download #%d is still '%s' — wait until it completes, then /dl again.", args.id, item.State)}
				}
				if first, ok := item.FirstFileID(); ok {
					url, err := b.torbox.RequestLink(ctx, kind, args.id, &first, false)
					if err == nil {
						return &torbox.Link{Kind: kind, ID: args.id, URL: url, Name: item.Name}, nil
					}
					lastErr = err
				}
			}
		}
	}
	if lastErr == nil {
		lastErr = &torbox.Error{Message: fmt.Sprintf("Could not get a link for #%d.", args.id)}
	}
	return nil, lastErr
}

// downloadText renders a link, or why there is none yet.
func downloadText(link *torbox.Link, public string, proxyEnabled bool) string {
	url := public
	if url == "" && !proxyEnabled {
		url = link.URL
	}
	title := "LINK NOT READY"
	if url != "" {
		title = "DOWNLOAD LINK"
	}
	name := link.Name
	if name == "" {
		name = "Unknown"
	}

	lines := []string{header(title), "", codeBlock(name), "ID: " + codeBlock(itoa(link.ID)), ""}
	switch {
	case url != "":
		lines = append(lines, `<a href="`+html.EscapeString(url)+`">Download</a>`)
	case link.URL != "" && proxyEnabled:
		lines = append(lines, "Could not create the download link. Please try again later.")
	default:
		lines = append(lines, "Wait for the download to finish, then try again.")
	}
	lines = append(lines, "", codeBlock("/dl "+itoa(link.ID)))
	return strings.Join(lines, "\n")
}

// sendDownload replies with a link, in whichever authorized chat asked for it.
// Behind the proxy the link is a Worker link, never the TorBox CDN URL.
func (r *request) sendDownload(ctx context.Context, link *torbox.Link, zip bool, fileID *int64) {
	// The whole download opens the file page; one file asked for by ID gets a
	// direct link.
	public := r.bot.links.Page(link.Kind, link.ID, link.Name)
	if !zip {
		public = r.bot.links.Link(proxy.Target{
			Kind: link.Kind, ID: link.ID, CDNURL: link.URL, FileID: fileID,
			ItemName: link.Name, OwnerID: r.userID(),
		})
	}
	r.replyLogged(ctx, downloadText(link, public, r.bot.links != nil))
}
