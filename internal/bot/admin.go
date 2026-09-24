package bot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gotd/td/telegram/message"
	messagepeer "github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// LogFilePath is the log the /logs command uploads.
const LogFilePath = "logs/torbot.log"

// handleLogs sends the current log file to the owner.
func (r *request) handleLogs(ctx context.Context) {
	log := r.bot.log.With("chat_id", r.chatID(), "user_id", r.userID())
	log.Info("Logs requested")

	info, err := os.Stat(LogFilePath)
	if err != nil || info.Size() == 0 {
		r.replyLogged(ctx, "No logs yet")
		return
	}

	file, err := uploader.NewUploader(r.bot.api).FromPath(ctx, LogFilePath)
	if err != nil {
		log.Warn("Failed to upload log file", "err", err)
		r.replyLogged(ctx, "Error sending log file")
		return
	}

	_, err = r.bot.sender.Answer(r.entities, r.update).
		ReplyMsg(r.msg).
		Media(ctx, message.UploadedDocument(file).
			Filename(filepath.Base(LogFilePath)).
			MIME("text/plain").
			ForceFile(true))
	if err != nil {
		log.Warn("Failed to send log file", "err", err)
		r.replyLogged(ctx, "Error sending log file")
		return
	}
	log.Info("Logs sent", "bytes", info.Size())
}

// authTarget resolves who an /auth or /unauth applies to: the user whose
// message was replied to, an explicit ID, or the current chat.
func (r *request) authTarget(ctx context.Context) (id int64, isUser, ok bool) {
	if userID := r.repliedUserID(ctx); userID != 0 {
		return userID, true, true
	}
	if len(r.args) > 0 {
		id, err := strconv.ParseInt(r.args[0], 10, 64)
		if err != nil {
			return 0, false, false
		}
		return id, false, true
	}
	return r.chatID(), false, true
}

// repliedMessage fetches the message this command replied to, or nil. MTProto
// carries only the replied-to ID in the reply header, so the message itself
// has to be fetched; the result is kept for the rest of the command.
func (r *request) repliedMessage(ctx context.Context) (*tg.Message, error) {
	if r.repliedFetch {
		return r.replied, r.repliedErr
	}
	r.repliedFetch = true
	r.replied, r.repliedErr = r.fetchReplied(ctx)
	return r.replied, r.repliedErr
}

func (r *request) fetchReplied(ctx context.Context) (*tg.Message, error) {
	header, ok := r.msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || header.ReplyToMsgID == 0 {
		return nil, nil
	}

	inputPeer, err := messagepeer.EntitiesFromUpdate(r.entities).ExtractPeer(r.msg.PeerID)
	if err != nil {
		return nil, err
	}
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: header.ReplyToMsgID}}

	var messages tg.MessagesMessagesClass
	if channel, ok := inputPeer.(*tg.InputPeerChannel); ok {
		messages, err = r.bot.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			ID:      ids,
		})
	} else {
		messages, err = r.bot.api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, err
	}

	replied, ok := messages.AsModified()
	if !ok {
		return nil, nil
	}
	for _, m := range replied.GetMessages() {
		if msg, ok := m.(*tg.Message); ok {
			return msg, nil
		}
	}
	return nil, nil
}

// repliedUserID returns the sender of the message this command replied to,
// or 0.
func (r *request) repliedUserID(ctx context.Context) int64 {
	msg, err := r.repliedMessage(ctx)
	if err != nil {
		r.bot.log.Warn("Failed to fetch replied message", "err", err)
		return 0
	}
	if msg == nil {
		return 0
	}
	if from, ok := msg.FromID.(*tg.PeerUser); ok {
		return from.UserID
	}
	// A message in a private chat carries no FromID.
	if peer, ok := msg.PeerID.(*tg.PeerUser); ok {
		return peer.UserID
	}
	return 0
}

// userSuffix labels replies whose target came from a replied-to message, so it
// is obvious a user was authorized rather than a chat.
func userSuffix(isUser bool) string {
	if isUser {
		return " [User]"
	}
	return ""
}

func (r *request) handleAuth(ctx context.Context) {
	id, isUser, ok := r.authTarget(ctx)
	if !ok {
		r.replyLogged(ctx, "Please provide a valid numeric ID.")
		return
	}

	if id == r.bot.cfg.OwnerID || r.bot.cfg.IsAuthorizedChat(id) {
		r.replyLogged(ctx, "Already authorized!"+userSuffix(isUser))
		return
	}

	added, err := r.bot.store.Authorize(id, r.userID())
	if err != nil {
		r.bot.log.Error("Failed to authorize", "id", id, "err", err)
		r.replyLogged(ctx, "Error saving authorization")
		return
	}
	if !added {
		r.replyLogged(ctx, "Already authorized!"+userSuffix(isUser))
		return
	}

	r.bot.log.Info("Authorized", "id", id, "by", r.userID())
	r.replyLogged(ctx, "Authorized "+codeBlock(itoa(id))+userSuffix(isUser))
}

func (r *request) handleUnauth(ctx context.Context) {
	id, isUser, ok := r.authTarget(ctx)
	if !ok {
		r.replyLogged(ctx, "Please provide a valid numeric ID.")
		return
	}

	if id == r.bot.cfg.OwnerID {
		r.replyLogged(ctx, "Bot admin!"+userSuffix(isUser))
		return
	}
	// An .env entry would come back on the next start, so say where it lives.
	if r.bot.cfg.IsAuthorizedChat(id) {
		r.replyLogged(ctx, "Authorized in .env, remove it there"+userSuffix(isUser))
		return
	}

	removed, err := r.bot.store.Revoke(id)
	if err != nil {
		r.bot.log.Error("Failed to revoke authorization", "id", id, "err", err)
		r.replyLogged(ctx, "Error saving authorization")
		return
	}
	if !removed {
		r.replyLogged(ctx, "Already unauthorized!"+userSuffix(isUser))
		return
	}

	r.bot.log.Info("Revoked authorization", "id", id, "by", r.userID())
	r.replyLogged(ctx, "Unauthorized "+codeBlock(itoa(id))+userSuffix(isUser))
}
