package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/chat"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// voiceConversationID 是语音那条对话在接口里的编号（存储里是 brain.VoiceThread）。
const voiceConversationID = "voice"

// sendMessagePattern 是打字的路由（回答是 Server-Sent Events，手写挂载，不经生成的代码）。
const sendMessagePattern = "POST " + PathPrefix + "conversations/{conversationId}/messages"

// Chat 是打字对话（chat.Service 实现）。
type Chat interface {
	CheckThread(ctx context.Context, person, thread string) error
	Send(ctx context.Context, speaker chat.Speaker, thread string, message chat.Message, language string) (*chat.Turn, error)
	Upload(ctx context.Context, person, thread string, data []byte) (conversationstore.Attachment, error)
	Image(ctx context.Context, person, id string) ([]byte, error)
	Search(ctx context.Context, person, query string) ([]conversationstore.FoundTurn, error)
	Stop(ctx context.Context, person, thread, turnID string) error
	Threads(ctx context.Context, person string) ([]conversationstore.Thread, error)
	Create(ctx context.Context, person string, incognito bool) (conversationstore.Thread, error)
	Delete(ctx context.Context, person, thread string) error
	Restore(ctx context.Context, person, thread string) (conversationstore.Thread, error)
	Turns(ctx context.Context, person, thread string, before int64, limit int) ([]conversationstore.RecordedTurn, error)
}

// threadOf 把接口里的对话编号换成存储里的线程。
func threadOf(conversationID string) string {
	if conversationID == voiceConversationID {
		return brain.VoiceThread
	}
	return conversationID
}

func conversationOf(thread conversationstore.Thread) appapi.Conversation {
	updated := thread.UpdatedAt
	conversation := appapi.Conversation{ConversationId: thread.ID, Kind: appapi.Chat, Incognito: thread.Incognito,
		Title: optional(thread.Title), LastText: optional(brain.WithoutEmotionTags(thread.LastText)), UpdatedAt: &updated}
	return conversation
}

// ListConversations 实现 appapi.StrictServerInterface：语音那条在最前。
func (s *Server) ListConversations(ctx context.Context, _ appapi.ListConversationsRequestObject) (appapi.ListConversationsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	voice := appapi.Conversation{ConversationId: voiceConversationID, Kind: appapi.Voice}
	latest, err := s.opts.Chat.Turns(ctx, account.OwnerPerson, brain.VoiceThread, 0, 1)
	if err != nil {
		return s.internalError(ctx, "读语音的对话", err), nil
	}
	if len(latest) == 1 {
		voice.LastText = optional(lastReply(latest[0].Messages))
		if !latest[0].At.IsZero() {
			at := latest[0].At
			voice.UpdatedAt = &at
		}
	}
	threads, err := s.opts.Chat.Threads(ctx, account.OwnerPerson)
	if err != nil {
		return s.internalError(ctx, "读对话", err), nil
	}
	reply := appapi.ListConversations200JSONResponse{voice}
	for _, thread := range threads {
		reply = append(reply, conversationOf(thread))
	}
	return reply, nil
}

// CreateConversation 实现 appapi.StrictServerInterface。
func (s *Server) CreateConversation(ctx context.Context, request appapi.CreateConversationRequestObject) (appapi.CreateConversationResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	incognito := request.Body != nil && request.Body.Incognito != nil && *request.Body.Incognito
	thread, err := s.opts.Chat.Create(ctx, account.OwnerPerson, incognito)
	if err != nil {
		return s.internalError(ctx, "建对话", err), nil
	}
	return appapi.CreateConversation201JSONResponse(conversationOf(thread)), nil
}

// DeleteConversation 实现 appapi.StrictServerInterface。
func (s *Server) DeleteConversation(ctx context.Context, request appapi.DeleteConversationRequestObject) (appapi.DeleteConversationResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	err := s.opts.Chat.Delete(ctx, account.OwnerPerson, threadOf(request.ConversationId))
	switch {
	case errors.Is(err, chat.ErrVoiceThread):
		return s.fail(ctx, http.StatusConflict, appapi.ErrorCodeConflict, nil), nil
	case errors.Is(err, chat.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "删对话", err), nil
	}
	return appapi.DeleteConversation204Response{}, nil
}

// RestoreConversation 实现 appapi.StrictServerInterface。
func (s *Server) RestoreConversation(ctx context.Context, request appapi.RestoreConversationRequestObject) (appapi.RestoreConversationResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	thread, err := s.opts.Chat.Restore(ctx, account.OwnerPerson, threadOf(request.ConversationId))
	switch {
	case errors.Is(err, chat.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "恢复对话", err), nil
	}
	return appapi.RestoreConversation200JSONResponse(conversationOf(thread)), nil
}

// ListTurns 实现 appapi.StrictServerInterface：一页 page_size 轮，旧的在前。
func (s *Server) ListTurns(ctx context.Context, request appapi.ListTurnsRequestObject) (appapi.ListTurnsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	var before int64
	if request.Params.Before != nil {
		before = *request.Params.Before
	}
	recorded, err := s.opts.Chat.Turns(ctx, account.OwnerPerson, threadOf(request.ConversationId), before, pageSize)
	switch {
	case errors.Is(err, chat.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "读对话记录", err), nil
	}
	page := appapi.TurnPage{Turns: []appapi.Turn{}}
	for _, turn := range recorded {
		page.Turns = append(page.Turns, turnOf(turn))
	}
	if len(recorded) == pageSize && recorded[0].Seq > 1 {
		next := recorded[0].Seq
		page.NextBefore = &next
	}
	return appapi.ListTurns200JSONResponse(page), nil
}

// turnOf 是给 App 看的一轮：主人说的（系统附注不算）、haro 的回答（文字和调用了的工具，按顺序）。
func turnOf(turn conversationstore.RecordedTurn) appapi.Turn {
	reply := appapi.Turn{Seq: turn.Seq, Reply: []appapi.ReplyBlock{}}
	if !turn.At.IsZero() {
		at := turn.At
		reply.At = &at
	}
	var said []string
	for _, message := range turn.Messages {
		switch {
		case message.Role == brain.RoleUser && !message.Synthetic && message.Text != "":
			said = append(said, message.Text)
		case message.Role == brain.RoleAssistant:
			for _, call := range message.ToolCalls {
				name := call.Name
				reply.Reply = append(reply.Reply, appapi.ReplyBlock{Type: appapi.ReplyBlockTypeTool, Tool: &name})
			}
			if text := brain.WithoutEmotionTags(message.Text); text != "" { // 历史里存的是模型的原话（含情绪标记）
				reply.Reply = append(reply.Reply, appapi.ReplyBlock{Type: appapi.ReplyBlockTypeText, Text: &text})
			}
		}
	}
	if len(said) > 0 {
		text := strings.Join(said, "\n")
		reply.Said = &text
	}
	for _, message := range turn.Messages {
		if message.Role == brain.RoleUser && message.Attachment != "" {
			attachment := message.Attachment
			reply.Attachment = &attachment
		}
	}
	return reply
}

// lastReply 是一轮里 haro 最后的回答（去掉情绪标记）。
func lastReply(messages []brain.ChatMessage) string {
	answer := ""
	for _, message := range messages {
		if text := brain.WithoutEmotionTags(message.Text); message.Role == brain.RoleAssistant && text != "" {
			answer = text
		}
	}
	return answer
}

// StopTurn 实现 appapi.StrictServerInterface。
func (s *Server) StopTurn(ctx context.Context, request appapi.StopTurnRequestObject) (appapi.StopTurnResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	err := s.opts.Chat.Stop(ctx, account.OwnerPerson, threadOf(request.ConversationId), request.TurnId)
	switch {
	case errors.Is(err, chat.ErrTurnNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "停止这一轮", err), nil
	}
	return appapi.StopTurn204Response{}, nil
}

// sendMessage 是打字：验令牌（和契约里要登录的接口一样）、按契约校验请求体，开一轮，把回答以 Server-Sent Events 写回去。
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	account, ok := s.bearerAccount(w, r)
	if !ok {
		return
	}
	if s.opts.Chat == nil {
		s.writeProblem(w, r, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
		return
	}
	var message appapi.MessageSend
	if err := json.NewDecoder(r.Body).Decode(&message); err != nil || strings.TrimSpace(message.Text) == "" ||
		len([]rune(message.Text)) > maxMessageRunes {
		s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "text"})
		return
	}
	speaker := chat.Speaker{Owner: account.OwnerPerson, Guest: account.GuestPerson, Admin: account.Role == accounts.RoleAdmin}
	language := languageFrom(r.Context())
	attachment := ""
	if message.Attachment != nil {
		attachment = *message.Attachment
	}
	turn, err := s.opts.Chat.Send(r.Context(), speaker, threadOf(r.PathValue("conversationId")),
		chat.Message{Text: message.Text, Attachment: attachment}, language)
	switch {
	case errors.Is(err, chat.ErrAttachmentNotFound), errors.Is(err, chat.ErrNoImages):
		s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "attachment"})
		return
	case errors.Is(err, chat.ErrNotFound):
		s.writeProblem(w, r, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
		return
	case errors.Is(err, chat.ErrBusy):
		s.writeProblem(w, r, http.StatusConflict, appapi.ErrorCodeConflict, nil)
		return
	case err != nil:
		s.logger.Error("开一轮打字出错", "err", err)
		s.writeProblem(w, r, http.StatusInternalServerError, appapi.ErrorCodeInternal, nil)
		return
	}
	defer turn.Detach() // App 断开：这一轮照样答完、存好
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-turn.Events():
			if !open {
				return
			}
			data, err := json.Marshal(s.chatEventOf(r.Context(), event))
			if err != nil {
				return
			}
			if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// maxMessageRunes 是一句最多几个字（和契约 MessageSend.text 的 maxLength 一致，测试守着）。
const maxMessageRunes = 4000

// chatEventOf 是给 App 的事件。
func (s *Server) chatEventOf(ctx context.Context, event chat.Event) appapi.ChatEvent {
	out := appapi.ChatEvent{Type: appapi.ChatEventType(event.Type)}
	switch event.Type {
	case chat.EventStarted:
		out.TurnId = &event.TurnID
	case chat.EventText:
		out.Text = &event.Text
	case chat.EventDone:
		status := appapi.ChatEventStatusCompleted
		switch event.Status {
		case realtimeproto.StatusCancelled:
			status = appapi.ChatEventStatusCancelled
		case realtimeproto.StatusIncomplete:
			status = appapi.ChatEventStatusIncomplete
		}
		out.Status = &status
	case chat.EventFailed:
		code := appapi.ErrorCodeGenerationFailed
		if event.Reason == "" {
			code = appapi.ErrorCodeInternal
		}
		message := s.texts.say(languageFrom(ctx), "problem."+string(code), nil)
		out.Code, out.Message = &code, &message
	}
	return out
}

// 图片：上传、取回（二进制，手写挂载）。
const (
	uploadPattern     = "POST " + PathPrefix + "conversations/{conversationId}/attachments"
	attachmentPattern = "GET " + PathPrefix + "attachments/{attachmentId}"
	// attachmentMaxBytes 是上传的一张图片最多多少字节（client-config 给 App）。
	attachmentMaxBytes = 10 << 20
)

// uploadedTypes 是收的图片格式（和契约 uploadAttachment 的请求体一致，测试守着）。
var uploadedTypes = []string{"image/jpeg", "image/png", "image/webp"}

// uploadAttachment 是在一条对话里传一张图片：请求体就是图片。
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	account, ok := s.bearerAccount(w, r)
	if !ok {
		return
	}
	if s.opts.Chat == nil {
		s.writeProblem(w, r, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
		return
	}
	data, ok := s.readImage(w, r)
	if !ok {
		return
	}
	attachment, err := s.opts.Chat.Upload(r.Context(), account.OwnerPerson, threadOf(r.PathValue("conversationId")), data)
	switch {
	case errors.Is(err, chat.ErrNotFound):
		s.writeProblem(w, r, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
		return
	case errors.Is(err, attachments.ErrUnsupported):
		s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "image"})
		return
	case errors.Is(err, chat.ErrNoImages):
		s.writeProblem(w, r, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
		return
	case err != nil:
		s.logger.Error("存图片出错", "err", err)
		s.writeProblem(w, r, http.StatusInternalServerError, appapi.ErrorCodeInternal, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(appapi.AttachmentInfo{AttachmentId: attachment.ID, Width: attachment.Width, Height: attachment.Height})
}

// readImage 读请求体里的一张图片（聊天的图片、小票共用）：格式不在 uploadedTypes 里、超过上限的回 400。
func (s *Server) readImage(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !slices.Contains(uploadedTypes, media) {
		s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "Content-Type"})
		return nil, false
	}
	data, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		s.writeProblem(w, r, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "size"})
		return nil, false
	case err != nil:
		return nil, false
	}
	return data, true
}

// writeImage 回一张存好的图片（都是 JPEG；只有本人取得到，浏览器可以缓存一天）。
func writeImage(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// getAttachment 是取回自己的一张图片。
func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	account, ok := s.bearerAccount(w, r)
	if !ok {
		return
	}
	if s.opts.Chat == nil {
		s.writeProblem(w, r, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
		return
	}
	data, err := s.opts.Chat.Image(r.Context(), account.OwnerPerson, r.PathValue("attachmentId"))
	switch {
	case errors.Is(err, chat.ErrNotFound), errors.Is(err, chat.ErrNoImages):
		s.writeProblem(w, r, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
		return
	case err != nil:
		s.logger.Error("取图片出错", "err", err)
		s.writeProblem(w, r, http.StatusInternalServerError, appapi.ErrorCodeInternal, nil)
		return
	}
	writeImage(w, data)
}

// SearchTurns 实现 appapi.StrictServerInterface。
func (s *Server) SearchTurns(ctx context.Context, request appapi.SearchTurnsRequestObject) (appapi.SearchTurnsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Chat == nil {
		return s.notImplemented(ctx), nil
	}
	found, err := s.opts.Chat.Search(ctx, account.OwnerPerson, request.Params.Q)
	if err != nil {
		return s.internalError(ctx, "搜对话", err), nil
	}
	reply := appapi.SearchTurns200JSONResponse{}
	for _, turn := range found {
		id := turn.Thread
		if id == brain.VoiceThread {
			id = voiceConversationID
		}
		reply = append(reply, appapi.FoundTurn{ConversationId: id, Turn: turnOf(turn.RecordedTurn)})
	}
	return reply, nil
}
