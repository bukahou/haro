package brain

import (
	"context"
	"errors"
	"fmt"
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
	wait := time.NewTimer(transcriptionWaitTimeout)
	defer wait.Stop()
	select {
	case <-job.done:
	case <-ctx.Done():
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	case <-wait.C: // 识别服务既没给结果也没报失败（例如卡住了）：这一轮不再等
		s.takeJob(job.item)
		return s.failed(realtimeproto.ReasonTranscriptionFailed, "语音转文字",
			fmt.Errorf("等了 %v 还没有识别结果", transcriptionWaitTimeout))
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
	record := newTurnRecord(job.text)
	situation, input := s.userTurn(job.text, record) // 现在的情况单独成一条、不进历史；历史里只存原话
	defer func() {
		for _, round := range record.rounds {
			for _, call := range round.calls {
				tools = append(tools, call.Name)
			}
		}
	}()
	history := append(s.historyWithInstructions(), situation...)
	answer, usage, err := s.speakAnswer(ctx, history, input, cameraView{request: job.text}, record)
	return s.finishAnswer(ctx, record, answer, usage, err)
}

// finishAnswer 按这一轮怎么结束收尾（写进历史），给出 response.done。
func (s *Session) finishAnswer(ctx context.Context, record *turnRecord, answer string, usage *ReplyUsage,
	err error) realtimeproto.ResponseDone {
	switch {
	case ctx.Err() != nil:
		s.closeTurn(record, turnInterrupted)
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusCancelled}
	case err != nil:
		var spoken SpokenError
		if errors.As(err, &spoken) && answer == "" && record.executedTools() == 0 { // 例如额度用完了：直接告诉主人
			s.logger.Warn("模型不可用，告诉主人原因", "err", err)
			return s.speakFixedReply(ctx, spoken.SpokenMessage())
		}
		s.closeTurn(record, turnFailed)
		return s.failed(realtimeproto.ReasonGenerationFailed, "生成回答", err)
	}
	s.closeTurn(record, turnCompleted)
	done := realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	if usage != nil {
		done.Usage = &realtimeproto.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.OutputTokens}
	}
	return done
}

// transcriptionWaitTimeout：判定说完之后等识别结果最多这么久。识别一句一般不到 0.1 秒；
// 识别服务断开时等的回答立刻失败，这个期限只兜「它还连着却一直不给结果」的情况。
var transcriptionWaitTimeout = 10 * time.Second

// wakeAcknowledgement 是只叫了唤醒词时的回答。
const wakeAcknowledgement = "我在。"

// speakFixedReply 不经过大模型，直接合成一句固定的话。
func (s *Session) speakFixedReply(ctx context.Context, text string) realtimeproto.ResponseDone {
	text = stripEmotionTags(text)
	if err := s.speakUtterance(ctx, 0, 0, Utterance{Text: text, Emotion: s.voiceEmotion(EmotionDefault),
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

// sentence 是切好的一句话，带着它属于这一轮回答的第几段（被打断时按段算听到了什么）。
type sentence struct {
	text  string
	round int
}

// speakAnswer 并行执行「模型流式生成 → 切句」与「逐句合成 → 发给终端」，返回这一轮说的全部文字。
// 这一轮的进展（工具往来、最后一段话）随时记进 record，不管怎么结束都由调用方收尾。
func (s *Session) speakAnswer(ctx context.Context, history []ChatMessage, input ChatMessage, view cameraView,
	record *turnRecord) (string, *ReplyUsage, error) {
	group, groupCtx := errgroup.WithContext(ctx)
	sentences := make(chan sentence, 4)
	var answer strings.Builder
	var usage *ReplyUsage

	group.Go(func() error {
		defer close(sentences)
		var err error
		usage, err = s.generateSentences(groupCtx, history, input, view, sentences, &answer, record)
		return err
	})
	group.Go(func() error {
		index := 0
		emotion := EmotionDefault // 情绪标记管到下一个标记为止
		for next := range sentences {
			var text string
			text, emotion = takeEmotion(next.text, emotion)
			if !hasSpokenText(text) { // 只有标记（或只剩标点）：情绪留给下一句
				continue
			}
			if err := s.speakUtterance(groupCtx, index, next.round, Utterance{Text: text, Emotion: s.voiceEmotion(emotion),
				Language: detectLanguage(text)}); err != nil {
				return err
			}
			index++
		}
		return nil
	})
	err := group.Wait()
	return strings.TrimSpace(answer.String()), usage, err
}

// generateSentences 拉取模型的流式输出，切好的句子（带段号）写进 sentences。
// 模型要求调用工具时：先把调用前说的话说出去 → 执行 → 把结果交回模型 → 再生成（最多 maxToolRounds 轮）；
// 需要确认的工具不执行，改为问用户「确定要删掉吗」，等下一轮回答。
// 工具往来和最后一段话随时记进 record（每个工具一做完就记，之后被打断、出错也不丢）。
func (s *Session) generateSentences(ctx context.Context, history []ChatMessage, input ChatMessage, view cameraView,
	sentences chan<- sentence, answer *strings.Builder, record *turnRecord) (*ReplyUsage, error) {
	ctx, span := tracer.Start(ctx, "brain.llm.stream")
	defer span.End()
	started := time.Now()
	segmenter := s.parts.NewSegmenter()
	var usage *ReplyUsage
	sawToken, sawSentence := false, false

	// 段号：和 turnRecord 一致——调用了工具的那次请求，段号就是它在 record.rounds 里的位置；
	// 最后一段（不再调用工具、直接念专家那句、问确认）的段号是之前工具往来的段数。从已经记下的段之后接着编（确认删除时先执行了一段）
	baseRound := len(record.rounds)
	segment := baseRound
	send := func(batch []string) error {
		for _, text := range batch {
			if !sawSentence {
				sawSentence = true
				elapsed := time.Since(started)
				span.AddEvent("first_sentence")
				timeToFirstSentence.Record(ctx, elapsed.Seconds())
				s.logger.Info("模型切出第一句", "elapsed", elapsed.Round(time.Millisecond))
			}
			select {
			case sentences <- sentence{text: text, round: segment}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	messages := append(history, input)
	inputIndex := len(history) // 这一轮主人那句话在 messages 里的位置（请更强的模型重答时在它上面做标记）
	specs := s.toolSpecs()
	for round := 0; ; round++ {
		segment = baseRound + round
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
				return usage.add(roundUsage), err
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
					return usage.add(roundUsage), err
				}
			}
			calls = append(calls, delta.ToolCalls...)
			if delta.Usage != nil {
				roundUsage = delta.Usage
			}
		}
		usage = usage.add(roundUsage)
		if len(calls) == 0 {
			record.final.WriteString(roundText.String())
			break
		}
		s.assignToolCallIDs(calls)
		if s.needsConfirmation(calls) {
			s.setPendingConfirmation(calls)
			record.askedConfirmation = true
			question := confirmationQuestion(calls)
			answer.WriteString(question)
			record.final.WriteString(roundText.String() + question)
			if err := send(segmenter.Push(question)); err != nil {
				return usage, err
			}
			break
		}
		// 调用工具前说的话（「我查一下」）先说出去：工具执行时主人就能听到，也不和之后的回答拼成一句
		if err := send(segmenter.Flush()); err != nil {
			return usage, err
		}
		toolRoundIndex := record.beginRound(roundText.String(), calls)
		exchange := []ChatMessage{{Role: RoleAssistant, Text: roundText.String(), ToolCalls: calls}}
		var frameMessages []ChatMessage // 看画面：图片只能放在用户消息里，排在所有工具结果之后
		var directAnswer string         // 专家那句直接当这一轮的回答（不再请求前台模型）
		switched := false
		for _, call := range calls {
			if ctx.Err() != nil { // 被打断：后面的调用不再执行（收尾时记成「没有执行」）
				break
			}
			var result string
			switch call.Name {
			case switchPersonaTool:
				result = s.switchPersona(call.Arguments)
				switched = true
			case askStrongerTool:
				result = s.escalate(&messages[inputIndex])
				record.rememberAs(toolRoundIndex, call, escalatedRecord)
			case lookAtCameraTool:
				look := s.lookAtCamera(ctx, view)
				direct := look.speakDirectly(calls)
				if direct {
					directAnswer = look.report.Spoken
				}
				result = look.toolResult(direct)
				record.rememberAs(toolRoundIndex, call, look.historyResult(direct))
				frames := look.frameMessages()
				frameMessages = append(frameMessages, frames...)
				record.rounds[toolRoundIndex].frameShown = len(frames) > 0
			default:
				result = s.callTool(ctx, call)
			}
			record.recordResult(toolRoundIndex, call, result)
			exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
		}
		if ctx.Err() != nil {
			return usage, ctx.Err()
		}
		exchange = append(exchange, frameMessages...)
		if switched && len(messages) > 0 && messages[0].Role == RoleSystem { // 换了性格：这次回答剩下的部分就按新性格说
			messages[0].Text = s.systemPrompt()
		}
		messages = append(messages, exchange...)
		if directAnswer != "" {
			// 前台模型转述时会扩写、重复调用前说过的话，字数由专家那边保证：这一轮直接念专家那句（算作最后一段）
			s.logger.Info("看画面：直接念专家那句", "chars", utf8.RuneCountInString(directAnswer))
			segment++
			answer.WriteString(directAnswer)
			record.final.WriteString(directAnswer)
			if err := send(segmenter.Push(directAnswer)); err != nil {
				return usage, err
			}
			break
		}
	}
	if err := send(segmenter.Flush()); err != nil {
		return usage, err
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
	return usage, nil
}

// speakUtterance 合成一句并边合成边发给终端：先发这句的文字，再逐段发音频。round 是这句属于这一轮回答的第几段。
// 合成失败时日志写明是语音合成这一环（没有备用语音合成，出问题只能看日志）。
func (s *Session) speakUtterance(ctx context.Context, index, round int, utterance Utterance) error {
	ctx, span := tracer.Start(ctx, "brain.synthesize")
	defer span.End()
	started := time.Now()
	s.mu.Lock()
	noSpeaker := s.noSpeaker
	s.mu.Unlock()
	if noSpeaker { // 终端没有音箱：只发文字，不占用语音合成
		s.markSpoken(utterance.Text, round)
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
			s.markSpoken(utterance.Text, round)
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
