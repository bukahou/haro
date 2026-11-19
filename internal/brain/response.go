package brain

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// transcriptionJob 是一句话的识别任务：识别流报「说完了」时登记，识别结果到达时完成。
type transcriptionJob struct {
	item    string // 识别流给这句话的编号（终端日志里同一个编号）
	done    chan struct{}
	text    string
	err     error
	started time.Time
	// 声纹：speakerChecked 为 true 时比对过，speaker 为空表示不是登记的人
	speaker        string
	speakerChecked bool
}

// runResponse 执行一次回答：开始时发 response.created，结束时恰好发出一个 response.done。
func (s *Session) runResponse(ctx context.Context, job *transcriptionJob) {
	s.emit(realtimeproto.ResponseCreated{})
	s.mu.Lock()
	s.spoken, s.truncateAtMs, s.answeredInResponse, s.responseItem = nil, -1, false, job.item // 新的回答：重新记播出的句子
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancelResponse()
		s.cancelResponse = nil
		// 回答已经记进历史之后才收到截断（打断发生在生成刚结束的一瞬间）：回头改
		s.truncateLastAnswerLocked()
		s.mu.Unlock()
	}()
	done := s.respond(ctx, job)
	s.emit(done)
}

func (s *Session) respond(ctx context.Context, job *transcriptionJob) (done realtimeproto.ResponseDone) {
	if job == nil {
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusIncomplete, Reason: realtimeproto.ReasonNoInputAudio}
	}
	var tools []string // 这一轮调用了哪些工具（按调用顺序）
	defer func() {
		s.logger.Info("一轮结束", "item", job.item, "status", done.Status, "reason", done.Reason, "tools", tools,
			"camera", slices.Contains(tools, lookAtCameraTool), "elapsed", since(job.started))
	}()
	select {
	case <-job.done:
	case <-ctx.Done():
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	}
	if job.err != nil {
		return s.failed(realtimeproto.ReasonTranscriptionFailed, "语音转文字", job.err)
	}
	s.emit(realtimeproto.InputAudioTranscriptionCompleted{Transcript: job.text, Speaker: job.speaker,
		SpeakerChecked: job.speakerChecked})
	if job.speakerChecked && job.speaker == "" { // 不是登记的人说的：不回答
		s.logger.Info("不是登记的人说的，不回答", "text", job.text)
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusIncomplete, Reason: realtimeproto.ReasonUnknownSpeaker}
	}
	if job.speakerChecked { // 对话跟人走：认出是谁就接这个人的对话
		s.useConversationOf(job.speaker)
	} else {
		s.useConversationOf(SharedPerson)
	}
	s.mu.Lock()
	justWoken := s.wakePending
	s.wakePending = false
	s.mu.Unlock()
	if !hasSpokenText(job.text) {
		if justWoken { // 只叫了唤醒词：不走大模型，直接回一句
			return s.speakFixedReply(ctx, wakeAcknowledgement)
		}
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusIncomplete, Reason: realtimeproto.ReasonEmptyTranscript}
	}
	if pending := s.takePendingConfirmation(); pending != nil { // 上一轮问了「确定要删掉吗」
		if done, handled := s.handleConfirmation(ctx, job.text, pending); handled {
			return done
		}
	}

	// 画面不直接附在这句话上：模型需要看时调用 look_at_camera，闲聊时手里就没有画面
	input := s.userTurn(job.text) // 带上现在的情况；历史里只存原话（remember 用 job.text）
	answer, toolTrail, usage, err := s.speakAnswer(ctx, s.historyWithInstructions(), input, cameraView{request: job.text})
	if ctx.Err() != nil {
		s.rememberInterrupted(job.text) // 被打断：只记用户听到的部分
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	}
	if err != nil {
		var spoken SpokenError
		if errors.As(err, &spoken) && answer == "" { // 例如额度用完了：直接告诉主人
			s.logger.Warn("模型不可用，告诉主人原因", "err", err)
			return s.speakFixedReply(ctx, spoken.SpokenMessage())
		}
		return s.failed(realtimeproto.ReasonGenerationFailed, "生成回答", err)
	}
	for _, msg := range toolTrail {
		for _, call := range msg.ToolCalls {
			tools = append(tools, call.Name)
		}
	}
	s.remember(job.text, toolTrail, answer)
	done = realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	if usage != nil {
		done.Usage = &realtimeproto.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.OutputTokens}
	}
	return done
}

// wakeAcknowledgement 是只叫了唤醒词时的回答。
const wakeAcknowledgement = "我在。"

// speakFixedReply 不经过大模型，直接合成一句固定的话。
func (s *Session) speakFixedReply(ctx context.Context, text string) realtimeproto.ResponseDone {
	text = stripEmotionTags(text)
	if err := s.speakUtterance(ctx, 0, Utterance{Text: text, Emotion: s.voiceEmotion(EmotionDefault),
		Language: detectLanguage(text)}); err != nil {
		if ctx.Err() != nil {
			return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
		}
		return s.failed(realtimeproto.ReasonSynthesisFailed, "语音合成", err)
	}
	return realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
}

// hasSpokenText 判断识别结果里有没有字（只有标点、空白的算没有）。
func hasSpokenText(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// failed 结束一次出错的回答：先发错误（给人看的说明带上是哪一步），再发 status=failed、带原因码的 response.done。
func (s *Session) failed(reason realtimeproto.ResponseReason, step string, err error) realtimeproto.ResponseDone {
	code := realtimeproto.CodeServerError
	if reason == realtimeproto.ReasonTranscriptionFailed {
		code = realtimeproto.CodeTranscriptionFailed
	}
	s.emit(realtimeproto.Error{Code: code, Message: step + ": " + err.Error()})
	return realtimeproto.ResponseDone{Status: realtimeproto.StatusFailed, Reason: reason}
}

// cameraView 是看画面时要带上的这一轮信息：主人这一轮的原话（看画面的专家要知道主人问的是什么）。
type cameraView struct {
	request string
}

// speakAnswer 并行执行「模型流式生成 → 切句」与「逐句合成 → 发给终端」，返回完整回答文字。
func (s *Session) speakAnswer(ctx context.Context, history []ChatMessage, input ChatMessage, view cameraView) (string, []ChatMessage, *ReplyUsage, error) {
	group, groupCtx := errgroup.WithContext(ctx)
	sentences := make(chan string, 4)
	var answer strings.Builder
	var usage *ReplyUsage
	var toolTrail []ChatMessage

	group.Go(func() error {
		defer close(sentences)
		var err error
		usage, toolTrail, err = s.generateSentences(groupCtx, history, input, view, sentences, &answer)
		return err
	})
	group.Go(func() error {
		index := 0
		emotion := EmotionDefault // 情绪标记管到下一个标记为止
		for sentence := range sentences {
			var text string
			text, emotion = takeEmotion(sentence, emotion)
			if !hasSpokenText(text) { // 只有标记（或只剩标点）：情绪留给下一句
				continue
			}
			if err := s.speakUtterance(groupCtx, index, Utterance{Text: text, Emotion: s.voiceEmotion(emotion),
				Language: detectLanguage(text)}); err != nil {
				return err
			}
			index++
		}
		return nil
	})
	err := group.Wait()
	return strings.TrimSpace(answer.String()), toolTrail, usage, err
}

// generateSentences 拉取模型的流式输出，切好的句子写进 sentences。
// 模型要求调用工具时：执行 → 把结果交回模型 → 再生成（最多 maxToolRounds 轮）；
// 需要确认的工具不执行，改为问用户「确定要删掉吗」，等下一轮回答。
// 返回这一轮的工具往来（assistant 的 tool_calls + 各工具结果），供写进历史。
func (s *Session) generateSentences(ctx context.Context, history []ChatMessage, input ChatMessage, view cameraView,
	sentences chan<- string, answer *strings.Builder) (*ReplyUsage, []ChatMessage, error) {
	ctx, span := tracer.Start(ctx, "brain.llm.stream")
	defer span.End()
	started := time.Now()
	segmenter := s.parts.NewSegmenter()
	var usage *ReplyUsage
	sawToken, sawSentence := false, false

	send := func(batch []string) error {
		for _, sentence := range batch {
			if !sawSentence {
				sawSentence = true
				elapsed := time.Since(started)
				span.AddEvent("first_sentence")
				timeToFirstSentence.Record(ctx, elapsed.Seconds())
				s.logger.Info("模型切出第一句", "elapsed", elapsed.Round(time.Millisecond))
			}
			select {
			case sentences <- sentence:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	messages := append(history, input)
	inputIndex := len(history) // 这一轮主人那句话在 messages 里的位置（请更强的模型重答时在它上面做标记）
	var toolTrail []ChatMessage
	specs := s.toolSpecs()
	for round := 0; ; round++ {
		var tools []ToolSpec
		if round < maxToolRounds { // 最后一轮不再给工具，逼模型直接回答
			tools = specs
		}
		var calls []ToolCall
		var roundText strings.Builder
		var roundUsage *ReplyUsage
		for delta, err := range s.parts.Model.StreamReply(ctx, messages, tools) {
			if err != nil {
				recordSpanError(span, err)
				return nil, nil, err
			}
			if delta.Text != "" {
				if !sawToken {
					sawToken = true
					span.AddEvent("first_token")
					timeToFirstToken.Record(ctx, time.Since(started).Seconds())
				}
				answer.WriteString(delta.Text)
				roundText.WriteString(delta.Text)
				if err := send(segmenter.Push(delta.Text)); err != nil {
					return nil, nil, err
				}
			}
			calls = append(calls, delta.ToolCalls...)
			if delta.Usage != nil {
				roundUsage = delta.Usage
			}
		}
		usage = usage.add(roundUsage)
		if len(calls) == 0 {
			break
		}
		s.assignToolCallIDs(calls)
		if s.needsConfirmation(calls) {
			s.setPendingConfirmation(calls)
			question := confirmationQuestion(calls)
			answer.WriteString(question)
			if err := send(segmenter.Push(question)); err != nil {
				return nil, nil, err
			}
			break
		}
		exchange := []ChatMessage{{Role: RoleAssistant, Text: roundText.String(), ToolCalls: calls}}
		var frameMessages []ChatMessage // 看画面：图片只能放在用户消息里，排在所有工具结果之后
		switched := false
		for _, call := range calls {
			if call.Name == switchPersonaTool {
				exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID,
					Text: s.switchPersona(call.Arguments)})
				switched = true
				continue
			}
			if call.Name == askStrongerTool {
				exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID,
					Text: s.escalate(&messages[inputIndex])})
				continue
			}
			if call.Name == lookAtCameraTool {
				result, frameMessage := s.lookAtCamera(ctx, view)
				exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
				frameMessages = append(frameMessages, frameMessage...)
				continue
			}
			exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID,
				Text: s.callTool(ctx, call)})
		}
		exchange = append(exchange, frameMessages...)
		if switched && len(messages) > 0 && messages[0].Role == RoleSystem { // 换了性格：这次回答剩下的部分就按新性格说
			messages[0].Text = s.systemPrompt()
		}
		messages = append(messages, exchange...)
		toolTrail = append(toolTrail, exchange...)
	}
	if err := send(segmenter.Flush()); err != nil {
		return nil, nil, err
	}
	if usage != nil {
		span.SetAttributes(
			attribute.Int("haro.llm.prompt_tokens", usage.PromptTokens),
			attribute.Int("haro.llm.output_tokens", usage.OutputTokens),
			attribute.Int("haro.llm.cache_read_tokens", usage.CacheReadTokens),
			attribute.Int("haro.llm.cache_write_tokens", usage.CacheWriteTokens),
			attribute.Int("haro.llm.requests", usage.Requests),
			attribute.Int("haro.llm.thinking_chars", usage.ThinkingChars),
			attribute.Int64("haro.llm.load_ms", usage.LoadMillis))
		s.logger.Info("模型用量", "requests", usage.Requests, "prompt_tokens", usage.PromptTokens, "output_tokens", usage.OutputTokens,
			"cache_read_tokens", usage.CacheReadTokens, "cache_write_tokens", usage.CacheWriteTokens,
			"visible_chars", utf8.RuneCountInString(answer.String()), "thinking_chars", usage.ThinkingChars)
	}
	s.logger.Info("阶段耗时", "stage", "brain.llm.stream", "elapsed", since(started))
	return usage, toolTrail, nil
}

// speakUtterance 合成一句并边合成边发给终端：先发这句的文字，再逐段发音频。
// 合成失败时日志写明是语音合成这一环（没有备用语音合成，出问题只能看日志）。
func (s *Session) speakUtterance(ctx context.Context, index int, utterance Utterance) error {
	ctx, span := tracer.Start(ctx, "brain.synthesize")
	defer span.End()
	started := time.Now()
	s.mu.Lock()
	noSpeaker := s.noSpeaker
	s.mu.Unlock()
	if noSpeaker { // 终端没有音箱：只发文字，不占用语音合成
		s.markSpoken(utterance.Text)
		s.emit(realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: utterance.Text})
		span.SetAttributes(attribute.Bool("haro.segment.no_speaker", true))
		return nil
	}
	var total time.Duration
	var firstChunk time.Duration
	var err error
	for audio, chunkErr := range s.parts.Synthesizer.Synthesize(ctx, utterance) {
		if chunkErr != nil {
			err = chunkErr
			break
		}
		if len(audio.Data) == 0 {
			continue
		}
		if total == 0 {
			s.markSpoken(utterance.Text)
			firstChunk = time.Since(started)
			span.AddEvent("first_chunk")
			s.emit(realtimeproto.ResponseOutputAudioTranscriptDelta{Delta: utterance.Text})
		}
		s.emit(realtimeproto.ResponseOutputAudioDelta{Audio: audio})
		s.extendSpoken(audio.Duration().Milliseconds())
		total += audio.Duration()
	}
	if err == nil && total == 0 && ctx.Err() == nil {
		err = errors.New("语音合成没有返回声音")
	}
	span.SetAttributes(
		attribute.Int("haro.segment.index", index),
		attribute.Int("haro.segment.chars", utf8.RuneCountInString(utterance.Text)),
		attribute.String("haro.segment.emotion", string(utterance.Emotion)),
		attribute.String("haro.segment.language", string(utterance.Language)),
		attribute.Float64("haro.segment.first_chunk_seconds", firstChunk.Seconds()),
		attribute.Float64("haro.segment.audio_seconds", total.Seconds()))
	recordSpanError(span, err)
	if err != nil && ctx.Err() == nil {
		s.logger.Error("语音合成失败", "index", index, "text", utterance.Text, "err", err)
		return err
	}
	s.logger.Info("合成一句", "index", index, "emotion", emotionLabel(utterance.Emotion), "language", utterance.Language,
		"first_chunk", firstChunk.Round(time.Millisecond), "elapsed", since(started), "audio", total)
	return ctx.Err()
}

func recordSpanError(span trace.Span, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func since(started time.Time) time.Duration { return time.Since(started).Round(time.Millisecond) }
