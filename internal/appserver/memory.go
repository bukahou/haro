package appserver

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/backstory"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/research"
)

// researchListLimit 是列出最近几个调查。
const researchListLimit = 50

func (s *Server) backstoryOf(ctx context.Context, person string) (appapi.Backstory, error) {
	view, used, err := s.opts.Backstory.View(ctx, person)
	if err != nil {
		return appapi.Backstory{}, err
	}
	out := appapi.Backstory{Enabled: view.Enabled, UsedChars: used, MaxChars: backstory.MaxRunes, Items: []appapi.BackstoryItem{}}
	for _, item := range view.Items {
		entry := appapi.BackstoryItem{ItemId: item.ID, Text: item.Text, Edited: item.Edited, UpdatedAt: item.UpdatedAt}
		if item.SourceSeq > 0 {
			id := item.SourceThread
			if id == brain.VoiceThread {
				id = voiceConversationID
			}
			entry.Source = &appapi.TurnRef{ConversationId: id, Seq: item.SourceSeq}
		}
		out.Items = append(out.Items, entry)
	}
	return out, nil
}

// GetBackstory 实现 appapi.StrictServerInterface。
func (s *Server) GetBackstory(ctx context.Context, _ appapi.GetBackstoryRequestObject) (appapi.GetBackstoryResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Backstory == nil {
		return s.notImplemented(ctx), nil
	}
	out, err := s.backstoryOf(ctx, account.OwnerPerson)
	if err != nil {
		return s.internalError(ctx, "读背景记录池", err), nil
	}
	return appapi.GetBackstory200JSONResponse(out), nil
}

// UpdateBackstory 实现 appapi.StrictServerInterface。
func (s *Server) UpdateBackstory(ctx context.Context, request appapi.UpdateBackstoryRequestObject) (appapi.UpdateBackstoryResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Backstory == nil {
		return s.notImplemented(ctx), nil
	}
	if err := s.opts.Backstory.SetEnabled(ctx, account.OwnerPerson, request.Body.Enabled); err != nil {
		return s.internalError(ctx, "开关背景记录池", err), nil
	}
	out, err := s.backstoryOf(ctx, account.OwnerPerson)
	if err != nil {
		return s.internalError(ctx, "读背景记录池", err), nil
	}
	return appapi.UpdateBackstory200JSONResponse(out), nil
}

// EditBackstoryItem 实现 appapi.StrictServerInterface。
func (s *Server) EditBackstoryItem(ctx context.Context, request appapi.EditBackstoryItemRequestObject) (appapi.EditBackstoryItemResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Backstory == nil {
		return s.notImplemented(ctx), nil
	}
	if utf8.RuneCountInString(request.Body.Text) == 0 {
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "text"}), nil
	}
	err := s.opts.Backstory.Edit(ctx, account.OwnerPerson, request.ItemId, request.Body.Text)
	switch {
	case errors.Is(err, conversationstore.ErrBackstoryItemNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "改背景记录池", err), nil
	}
	out, err := s.backstoryOf(ctx, account.OwnerPerson)
	if err != nil {
		return s.internalError(ctx, "读背景记录池", err), nil
	}
	return appapi.EditBackstoryItem200JSONResponse(out), nil
}

// DeleteBackstoryItem 实现 appapi.StrictServerInterface。
func (s *Server) DeleteBackstoryItem(ctx context.Context, request appapi.DeleteBackstoryItemRequestObject) (appapi.DeleteBackstoryItemResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Backstory == nil {
		return s.notImplemented(ctx), nil
	}
	err := s.opts.Backstory.Delete(ctx, account.OwnerPerson, request.ItemId)
	switch {
	case errors.Is(err, conversationstore.ErrBackstoryItemNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "删背景记录池的一条", err), nil
	}
	return appapi.DeleteBackstoryItem204Response{}, nil
}

func noteOf(note notes.Note) appapi.Note {
	out := appapi.Note{NoteId: note.ID, Content: note.Content, Resident: note.Resident, CreatedAt: note.CreatedAt}
	if !note.DeletedAt.IsZero() {
		at := note.DeletedAt
		out.ForgottenAt = &at
	}
	return out
}

// ListNotes 实现 appapi.StrictServerInterface。
func (s *Server) ListNotes(ctx context.Context, _ appapi.ListNotesRequestObject) (appapi.ListNotesResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Memory == nil {
		return s.notImplemented(ctx), nil
	}
	listing, err := s.opts.Memory.List(ctx, account.OwnerPerson, true)
	if err != nil {
		return s.internalError(ctx, "读记过的事", err), nil
	}
	out := appapi.NoteList{Notes: []appapi.Note{}, Forgotten: []appapi.Note{}, Total: listing.Total}
	for _, note := range listing.Notes {
		out.Notes = append(out.Notes, noteOf(note))
	}
	for _, note := range listing.Forgotten {
		out.Forgotten = append(out.Forgotten, noteOf(note))
	}
	return appapi.ListNotes200JSONResponse(out), nil
}

// ForgetNote 实现 appapi.StrictServerInterface。
func (s *Server) ForgetNote(ctx context.Context, request appapi.ForgetNoteRequestObject) (appapi.ForgetNoteResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Memory == nil {
		return s.notImplemented(ctx), nil
	}
	_, err := s.opts.Memory.Forget(ctx, account.OwnerPerson, request.NoteId)
	switch {
	case errors.Is(err, notes.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "忘掉一条", err), nil
	}
	return appapi.ForgetNote204Response{}, nil
}

// RestoreNote 实现 appapi.StrictServerInterface。
func (s *Server) RestoreNote(ctx context.Context, request appapi.RestoreNoteRequestObject) (appapi.RestoreNoteResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Memory == nil {
		return s.notImplemented(ctx), nil
	}
	note, err := s.opts.Memory.Restore(ctx, account.OwnerPerson, request.NoteId)
	switch {
	case errors.Is(err, notes.ErrNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "找回一条", err), nil
	}
	return appapi.RestoreNote200JSONResponse(noteOf(note)), nil
}

func (s *Server) researchTaskOf(ctx context.Context, task research.Task) appapi.ResearchTask {
	return appapi.ResearchTask{TaskId: task.ID, Question: task.Question, Status: appapi.ResearchStatus(task.Status),
		StatusLabel: s.texts.say(languageFrom(ctx), "research."+string(task.Status), nil), Conclusion: optional(task.Conclusion),
		Sources: append([]string{}, task.Sources...), StopReason: optional(task.StopReason), CreatedAt: task.CreatedAt}
}

// ListResearchTasks 实现 appapi.StrictServerInterface。
func (s *Server) ListResearchTasks(ctx context.Context, _ appapi.ListResearchTasksRequestObject) (appapi.ListResearchTasksResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Research == nil {
		return s.notImplemented(ctx), nil
	}
	tasks, err := s.opts.Research.Tasks(ctx, account.OwnerPerson, researchListLimit)
	if err != nil {
		return s.internalError(ctx, "读调查", err), nil
	}
	out := appapi.ListResearchTasks200JSONResponse{}
	for _, task := range tasks {
		out = append(out, s.researchTaskOf(ctx, task))
	}
	return out, nil
}

// GetResearchTask 实现 appapi.StrictServerInterface。
func (s *Server) GetResearchTask(ctx context.Context, request appapi.GetResearchTaskRequestObject) (appapi.GetResearchTaskResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Research == nil {
		return s.notImplemented(ctx), nil
	}
	task, err := s.opts.Research.Task(ctx, account.OwnerPerson, request.TaskId)
	switch {
	case errors.Is(err, research.ErrTaskNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "读调查", err), nil
	}
	return appapi.GetResearchTask200JSONResponse(s.researchTaskOf(ctx, task)), nil
}
