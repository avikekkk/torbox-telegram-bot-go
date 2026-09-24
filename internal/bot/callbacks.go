package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	messagepeer "github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/tg"
)

// callback carries one inline-button press through its handler.
type callback struct {
	ctx     context.Context
	queryID int64
	userID  int64
	msgID   int
	peer    tg.InputPeerClass
	data    string
}

// onCallbackQuery routes an inline-button press to the flow that owns it.
func (b *Bot) onCallbackQuery(e tg.Entities, u *tg.UpdateBotCallbackQuery) error {
	data := string(u.Data)

	var handler func(*callback)
	switch {
	case strings.HasPrefix(data, "srch"):
		handler = b.onSearchCallback
	case strings.HasPrefix(data, "purge:"):
		handler = b.onPurgeCallback
	default:
		// A button from an older build, or one whose flow is gone: answer so
		// the client stops spinning.
		b.goHandle("callback", func(ctx context.Context) {
			b.log.Warn("Unhandled callback query", "user_id", u.UserID, "data", data)
			b.answerCallback(ctx, u.QueryID, "This button is no longer active.", false)
		})
		return nil
	}

	target, err := messagepeer.EntitiesFromUpdate(e).ExtractPeer(u.Peer)
	if err != nil {
		b.log.Error("Could not resolve callback peer", "err", err)
		return nil
	}

	b.goHandle("callback", func(ctx context.Context) {
		handler(&callback{
			ctx:     ctx,
			queryID: u.QueryID,
			userID:  u.UserID,
			msgID:   u.MsgID,
			peer:    target,
			data:    data,
		})
	})
	return nil
}

func (b *Bot) answerCallback(ctx context.Context, queryID int64, text string, alert bool) {
	ctx, cancel := b.reporting(ctx)
	defer cancel()
	req := &tg.MessagesSetBotCallbackAnswerRequest{QueryID: queryID}
	if text != "" {
		req.SetMessage(text)
	}
	if alert {
		req.SetAlert(true)
	}
	if _, err := b.api.MessagesSetBotCallbackAnswer(ctx, req); err != nil {
		b.log.Warn("Failed to answer callback query", "err", err)
	}
}

// editLogged edits a message outside a command, treating a no-op edit as
// success.
func (b *Bot) editLogged(ctx context.Context, target tg.InputPeerClass, msgID int, text string) {
	if err := b.edit(ctx, target, msgID, text, nil); err != nil && !isNotModified(err) {
		b.log.Warn("Failed to edit message", "err", err)
	}
}

func newToken(length int) (string, error) {
	raw := make([]byte, (length+1)/2)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw)[:length], nil
}
