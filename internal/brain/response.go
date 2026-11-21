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
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
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
	tentative      string        // 叫醒这句的声纹只过了唤醒的门槛、没确认时，像是谁
	language       string        // 识别出的语种（固定的话用哪种语言念）
	wake, bargeIn  bool          // 叫醒后的第一句；在 haro 说话时打断说的
	audio          time.Duration // 这句话多长

	speakerCheckFailed bool // 开着声纹、这句却算不出声纹（声纹的第三种结果，和没比对分开）
}

// runResponse 执行一次回答：开始时发 response.created，结束时恰好发出一个 response.done。
func (s *Session) runResponse(ctx context.Context, job *transcriptionJob) {
	s.emit(realtimeproto.ResponseCreated{})
	s.mu.Lock()
	heard := s.pendingDelivered // 上一轮说完后一直没收到截断：主人听完了
	s.pendingDelivered = nil
	s.spoken, s.truncateAtMs, s.answeredInResponse, s.responseItem = nil, -1, false, job.item // 新的回答：重新记播出的句子
	s.mu.Unlock()
	for _, delivered := range heard { // 在这一轮附上情况之前：告诉过的不再附上
		delivered()
	}
	defer func() {
		s.mu.Lock()
		s.cancelResponse()
		s.cancelResponse = nil
		// 回答已经记进历史之后才收到截断（打断发生在生成刚结束的一瞬间）：回头改
		changed := s.playbackCutLocked()
		conversation := s.conversation
		s.mu.Unlock()
		if changed {
			conversation.persist()
		}
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
	if job.speakerChecked && job.speaker == "" { // 不是登记的人说的
		if job.tentative != "" && job.wake && job.audio <= unverifiedShortUtterance { // 叫醒的短句：受限回答
			return s.speakRestricted(ctx, job)
		}
		s.logger.Info("不是登记的人说的，不回答", "text", job.text)
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusIncomplete, Reason: realtimeproto.ReasonUnknownSpeaker}
	}
	if job.speakerChecked { // 对话跟人走：认出是谁就接这个人的对话
		s.useConversationOf(job.speaker)
	} else {
		if job.speakerCheckFailed { // 开着声纹、这句却算不出来：和没比对一样处理，单独记一条
			s.logger.Warn("这句的声纹没算出来：按没比对处理（进全家共用的对话、照样回答）", "item", job.item,
				"speaker_check_failed", true)
		}
		s.useConversationOf(SharedPerson)
	}
	s.rememberLanguage(job.language)
	if !hasSpokenText(job.text) {
		return realtimeproto.ResponseDone{Status: realtimeproto.StatusIncomplete, Reason: realtimeproto.ReasonEmptyTranscript}
	}
	// 画面不直接附在这句话上：模型需要看时调用 look_at_camera，闲聊时手里就没有画面
	record := newTurnRecord(job.text)
	record.voiceVerified = job.speakerChecked && job.speaker != ""
	record.extraNotes = s.wakeNotes(job)
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
	case err != nil && errors.Is(err, ErrModelUnavailable) && answer == "" && record.executedTools() == 0:
		// 最低能力（第 7 段）：连不上就不调用任何模型，念一句固定的话：连不上 + 现在的时间（报时是顺带给的，不用判断主人问的是什么）
		s.logger.Warn("连不上模型服务：告诉主人，顺带报时", "err", err)
		s.mu.Lock()
		language := s.language
		s.mu.Unlock()
		return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, PhraseModelUnavailable,
			map[string]string{"time": time.Now().Format("15:04")}))
	case err != nil && errors.Is(err, ErrMonthlyCapReached) && answer == "" && record.executedTools() == 0 && s.canRaiseMonthlyCap():
		return s.answerOverBudget(ctx, record, err) // 上限到了：不回答，但主人可以口头临时提高（第 7 段）
	case err != nil:
		var spoken SpokenError
		if errors.As(err, &spoken) && answer == "" && record.executedTools() == 0 { // 例如额度用完了：直接告诉主人
			s.logger.Warn("模型不可用，告诉主人原因", "err", err)
			key, values := spoken.Phrase()
			s.mu.Lock()
			language := s.language
			s.mu.Unlock()
			return s.speakFixedReply(ctx, s.opts.Phrases.Say(language, key, values))
		}
		s.closeTurn(record, turnFailed)
		return s.failed(realtimeproto.ReasonGenerationFailed, "生成回答", err)
	}
	ending, done := turnCompleted, realtimeproto.ResponseDone{Status: realtimeproto.StatusCompleted}
	if record.truncated { // 到了输出上限：照「没说完」告诉终端（OpenAI Realtime 同样的状态和原因）
		ending, done = turnTruncated, realtimeproto.ResponseDone{Status: realtimeproto.StatusIncomplete, Reason: realtimeproto.ReasonMaxOutputTokens}
	}
	s.closeTurn(record, ending)
	if usage.InputKnown() && usage.OutputKnown() { // 用量不知道时不发（不编成 0）
		done.Usage = &realtimeproto.Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.OutputTokens}
	}
	return done
}

// transcriptionWaitTimeout：判定说完之后等识别结果最多这么久。识别一句一般不到 0.1 秒；
// 识别服务断开时等的回答立刻失败，这个期限只兜「它还连着却一直不给结果」的情况。
var transcriptionWaitTimeout = 10 * time.Second

// speakFixedReply 不经过大模型，直接合成一句固定的话。
func (s *Session) speakFixedReply(ctx context.Context, text string) realtimeproto.ResponseDone {
	text = stripEmotionTags(text)
	if err := s.speakUtterance(ctx, 0, 0, Utterance{Text: text, Emotion: s.voiceEmotion(EmotionDefault)}); err != nil {
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
	text    string
	round   int
	emotion Emotion // 这句用的语气（情绪标记管到下一个标记为止）
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
		for next := range sentences {
			if !hasSpokenText(next.text) { // 只剩标点：不念
				continue
			}
			if tag, ok := leadingUnknownTag(next.text); ok {
				s.logger.Warn("模型写了清单外的情绪标记：照原样念出", "tag", tag)
			}
			if err := s.speakUtterance(groupCtx, index, next.round, Utterance{Text: next.text, Emotion: s.voiceEmotion(next.emotion)}); err != nil {
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
// 模型要求调用工具时：先把调用前说的话说出去 → 执行 → 把结果交回模型 → 再生成（步数和死循环见 tool_steps.go）；
// 会破坏的工具先不执行：核对声音、存下参数（确认闸，见 confirmation.go），由模型用自己的话问主人，主人同意后再执行。
// 工具往来和最后一段话随时记进 record（每个工具一做完就记，之后被打断、出错也不丢）。
func (s *Session) generateSentences(ctx context.Context, history []ChatMessage, input ChatMessage, view cameraView,
	sentences chan<- sentence, answer *strings.Builder, record *turnRecord) (*ReplyUsage, error) {
	ctx, span := tracer.Start(ctx, "brain.llm.stream")
	defer span.End()
	started := time.Now()
	segmenter := s.parts.NewSegmenter()
	tags := &emotionStream{} // 情绪标记在进切句器之前认出来
	emotion := EmotionDefault
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
			case sentences <- sentence{text: text, round: segment, emotion: emotion}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	// speak 把模型的文字交给切句器；遇到情绪标记，先把前面的文字收尾成句，之后的句子换成新的语气
	speak := func(pieces []emotionPiece) error {
		for _, piece := range pieces {
			if !piece.tag {
				if err := send(segmenter.Push(piece.text)); err != nil {
					return err
				}
				continue
			}
			if err := send(segmenter.Flush()); err != nil {
				return err
			}
			emotion = piece.emotion
		}
		return nil
	}
	pushText := func(text string) error { return speak(tags.Push(text)) }
	flushText := func() error { // 剩下的文字都收尾成句（调用工具前、这一轮说完时）
		if err := speak(tags.Flush()); err != nil {
			return err
		}
		return send(segmenter.Flush())
	}

	messages := append(history, input)
	specs := s.toolSpecs()
	if record.restricted { // 声纹没确认的叫醒短句：不给工具
		specs = nil
	}
	// 这一轮先由前台回答；交给接手的代理后，剩下的请求都由它回答（带着同样的历史和系统提示词），步数从交接时重新算
	agent, handedOver, firstRound := s.parts.Front, false, 0
	ctx = startAgent(ctx, agent.Name)
	span.SetAttributes(semconv.GenAIOperationNameInvokeAgent, semconv.GenAIAgentName(agent.Name))
	defer func() { span.SetAttributes(semconv.GenAIAgentName(agent.Name)) }() // 交给了接手的代理时，记最后回答的那个
	offered := agent.offer(specs)
	steps, forceFinal := newToolSteps(agent), false
	for round := 0; ; round++ {
		segment = baseRound + round
		// 最后一步（或刚停下一个死循环）：工具定义照给（提示词缓存不失效）、但不许调用，逼模型直接回答
		request := ChatRequest{Messages: messages, Tools: offered, ForbidToolCalls: round-firstRound >= agent.maxSteps()-1 || forceFinal}
		var calls []ToolCall
		var roundText strings.Builder
		var roundUsage *ReplyUsage
		stop := StopUnknown // 这次请求为什么停：最后一段说了算
		for delta, err := range agent.Model.StreamReply(ctx, request) {
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
				if err := pushText(delta.Text); err != nil {
					return usage.add(roundUsage), err
				}
			}
			calls = append(calls, delta.ToolCalls...)
			if delta.Usage != nil {
				roundUsage = delta.Usage
			}
			stop = delta.Stop
		}
		usage = usage.add(roundUsage)
		if roundUsage.InputKnown() { // 不知道时不算（「上下文太长就提前压缩」对不报用量的服务不起作用，见下面的用量日志）
			record.contextTokens = max(record.contextTokens, roundUsage.PromptTokens)
		}
		if stop == StopUnknown { // 流结束了却没说为什么停：不当成说完，按出错收尾（说出口的照样记进历史）
			s.logger.Warn("模型的流结束了却没说为什么停：按出错收尾", "agent", agent.Name, "round", round, "tools", toolNames(calls))
			recordSpanError(span, ErrStopUnknown)
			return usage, ErrStopUnknown
		}
		if len(calls) > 0 && request.ForbidToolCalls { // 不许调用时还要调用：不执行，说了的话就是回答
			s.logger.Warn("模型在不许调用工具的一轮要求调用工具：不执行", "tools", toolNames(calls))
			calls = nil
		}
		if stop == StopMaxTokens { // 到了输出上限：说出去的就是这些（适配不交出被截断的工具调用）
			s.logger.Warn("回答到了模型的输出上限，没说完", "round", round, "output_tokens", outputTokens(roundUsage))
			record.final.WriteString(roundText.String())
			record.truncated = true
			break
		}
		if len(calls) == 0 {
			record.final.WriteString(roundText.String())
			break
		}
		s.assignToolCallIDs(calls)
		// 调用工具前说的话（「我查一下」）先说出去：工具执行时主人就能听到，也不和之后的回答拼成一句
		if err := flushText(); err != nil {
			return usage, err
		}
		toolRoundIndex := record.beginRound(roundText.String(), calls)
		exchange := []ChatMessage{{Role: RoleAssistant, Text: roundText.String(), ToolCalls: calls}}
		var frameMessages []ChatMessage // 看画面：图片只能放在用户消息里，排在所有工具结果之后
		var directAnswer string         // 专家那句直接当这一轮的回答（不再请求前台模型）
		switched, handOff := false, false
		steps.begin()
		for _, call := range calls {
			if count, loop := steps.repeated(call); loop { // 死循环：不执行，告诉模型为什么停，下一步不许再调用
				s.logger.Warn("同一个调用反复出现：不再执行", "tool", call.Name, "times", count)
				result := loopStoppedResult(count)
				record.recordResult(toolRoundIndex, call, result)
				exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
				forceFinal = true
				continue
			}
			if ctx.Err() != nil { // 被打断：后面的调用不再执行（收尾时记成「没有执行」）
				break
			}
			if !executable(agent, offered, record.restricted, call.Name) { // 这一步没给它的工具（照着历史里见过的名字调用）：不执行
				s.logger.Warn("模型调用了这一步没提供的工具：不执行", "tool", call.Name, "agent", agent.Name)
				result := notOfferedResult(call.Name)
				record.recordResult(toolRoundIndex, call, result)
				exchange = append(exchange, ChatMessage{Role: RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
				continue
			}
			var result string
			switch call.Name {
			case switchPersonaTool:
				result = s.switchPersona(call.Arguments)
				switched = true
			case askStrongerTool:
				var escalated bool
				result, escalated = s.escalate(handedOver)
				handOff = handOff || escalated
				record.rememberAs(toolRoundIndex, call, escalatedRecord)
			case confirmActionTool:
				result = s.confirmAction(ctx, record, call)
			case cancelActionTool:
				result = s.cancelAction(call)
			case lookAtCameraTool:
				in, err := DecodeArguments[lookAtCameraInput](call.Arguments)
				if err != nil { // 参数不对也照样看：问题只是帮专家看准，主人的原话照样交过去
					s.logger.Warn("看画面的参数不对：只按主人的原话看", "err", err)
				}
				look := s.lookAtCamera(ctx, view, in.Question)
				direct := look.speakDirectly(calls)
				if direct {
					directAnswer = look.report.Spoken
				}
				result = look.toolResult(direct)
				record.rememberAs(toolRoundIndex, call, look.historyResult(direct))
				frames := look.frameMessages()
				frameMessages = append(frameMessages, frames...)
				record.rounds[toolRoundIndex].frameShown = len(frames) > 0
				if len(frames) > 0 && s.parts.Handoff.frames() && !handedOver { // 画面交给接手的代理看
					handOff = true
				}
			default:
				if s.requiresConfirmation(call.Name) { // 会破坏的：先问主人（确认闸）
					result = s.holdForConfirmation(record, call)
				} else {
					result = s.callTool(ctx, record, call)
				}
			}
			record.recordResult(toolRoundIndex, call, result)
			s.keepOpenTurn(record) // 做完了一个工具：这一轮中途服务端重启的话，重启后照实接进历史
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
			if err := pushText(directAnswer); err != nil {
				return usage, err
			}
			break
		}
		if handOff && s.parts.Handoff.Agent != nil {
			next := *s.parts.Handoff.Agent
			delegated, err := EnterAgent(ctx, s.logger, next.Name, Handover)
			if err != nil {
				s.logger.Warn("交不出去：这一轮还由原来的代理回答", "err", err)
				continue
			}
			ctx, agent, handedOver, firstRound = delegated, next, true, round+1
			span.AddEvent("handoff", trace.WithAttributes(semconv.GenAIAgentName(agent.Name)))
			offered, steps, forceFinal = agent.offer(specs), newToolSteps(agent), false
		}
	}
	if err := flushText(); err != nil {
		return usage, err
	}
	s.recordTurnUsage(span, agent.Name, usage, utf8.RuneCountInString(answer.String()))
	s.logger.Info("阶段耗时", "stage", "brain.llm.stream", "elapsed", since(started))
	return usage, nil
}

// recordTurnUsage 把这一轮所有请求加起来的用量记进 span 和日志（每次请求各自的 span「chat <模型>」在 llm 包里记）。
// 不知道的 token 数不写（不编成 0）：模型服务不报用量时，「上下文太长就提前压缩」看不到上下文多大、不起作用，
// 历史只按轮数截断，日志里写明。
func (s *Session) recordTurnUsage(span trace.Span, agent string, usage *ReplyUsage, visibleChars int) {
	if usage == nil {
		return
	}
	span.SetAttributes(attribute.Int("haro.llm.requests", usage.Requests), attribute.Int("haro.llm.thinking_chars", usage.ThinkingChars))
	if !usage.InputKnown() {
		s.logger.Info("模型用量：模型服务没报 token 数，不能按上下文大小提前压缩（历史只按轮数截断）", "agent", agent,
			"requests", usage.Requests, "visible_chars", visibleChars, "thinking_chars", usage.ThinkingChars)
		return
	}
	span.SetAttributes(semconv.GenAIUsageInputTokens(usage.PromptTokens), semconv.GenAIUsageCacheReadInputTokens(usage.CacheReadTokens),
		semconv.GenAIUsageCacheCreationInputTokens(usage.CacheWriteTokens))
	if usage.OutputKnown() {
		span.SetAttributes(semconv.GenAIUsageOutputTokens(usage.OutputTokens))
	}
	s.logger.Info("模型用量", "agent", agent, "requests", usage.Requests, "prompt_tokens", usage.PromptTokens,
		"output_tokens", usage.OutputTokens, "output_known", usage.OutputKnown(),
		"cache_read_tokens", usage.CacheReadTokens, "cache_write_tokens", usage.CacheWriteTokens,
		"visible_chars", visibleChars, "thinking_chars", usage.ThinkingChars)
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
		attribute.Float64("haro.segment.first_chunk_seconds", firstChunk.Seconds()),
		attribute.Float64("haro.segment.audio_seconds", total.Seconds()))
	recordSpanError(span, err)
	if err != nil && ctx.Err() == nil {
		s.logger.Error("语音合成失败", "index", index, "text", utterance.Text, "err", err)
		return err
	}
	s.logger.Info("合成一句", "index", index, "emotion", emotionLabel(utterance.Emotion),
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

// toolNames 是一组调用的工具名（日志用）。
func toolNames(calls []ToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return names
}

// outputTokens 是这次请求输出了多少 token（日志用；不知道时为 0）。
func outputTokens(usage *ReplyUsage) int {
	if usage == nil {
		return 0
	}
	return usage.OutputTokens
}
