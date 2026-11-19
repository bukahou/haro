// Package vision 是看画面的幕后专家：前台模型不直接看图，要看画面时把画面和主人这一轮的原话交给更强的模型，
// 拿回两段结论（直接念给主人听的一句、看到的细节）交给前台（实现 brain.FrameExpert）。
//
// 专家只看画面和主人的原话：不带性格、不带对话记录，输入小（约 1.6k token），比让更强的模型接手整轮便宜得多。
// 需要联网核对时专家自己查：只有它看得到画面，才能按「画面比搜到的资料可靠」取舍搜索结果。
package vision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/bukahou/haro/internal/brain"
)

const (
	maxSearches    = 2                // 专家一次最多联网查几次
	examineTimeout = 25 * time.Second // 看一次（含联网查）最多多久；前台在等，超时就退回把画面交给前台
)

// Search 是专家联网查一次的做法（含额度与记账）；nil = 不能联网。
type Search func(ctx context.Context, query string) (string, error)

// Expert 看画面的幕后专家。
type Expert struct {
	model     brain.ChatModel // 已按用途记账、查额度
	modelName string          // 只用于日志
	search    Search
	logger    *slog.Logger
}

// NewExpert 创建专家；search 为 nil 时专家不能联网。
func NewExpert(model brain.ChatModel, modelName string, search Search, logger *slog.Logger) *Expert {
	return &Expert{model: model, modelName: modelName, search: search, logger: logger}
}

var _ brain.FrameExpert = (*Expert)(nil)

const (
	searchTool = "search_web"
	reportTool = "report_frame"
)

// searchArgs 是 search_web 的参数；reportArgs 是 report_frame 的参数。工具的参数定义由这两个结构体生成。
type searchArgs struct {
	Query string `json:"query" jsonschema:"搜索词，用外观特征加上画面里读到的字，例如「蓝色 包装盒 M.2 硬盘盒 Groovy」"`
}

type reportArgs struct {
	Spoken  string `json:"spoken"`
	Details string `json:"details" jsonschema:"看到的形状、颜色、材质、上面的字和图案，判断依据，哪些是画面上看到的、哪些是查到的（主人追问时用，不念给主人）"`
}

var (
	searchSpec = brain.ToolSpec{Name: searchTool,
		Description: "联网搜索一次，用来核对画面里的东西（例如确认产品名、型号）。",
		Parameters:  toolSchema[searchArgs](nil)}
	reportSpec = brain.ToolSpec{Name: reportTool,
		Description: "看完画面后调用一次，交出结论：说给主人听的话和细节分开交。",
		Parameters: toolSchema[reportArgs](map[string]string{"spoken": fmt.Sprintf("直接念给主人听的一两句，不超过 %d 个字，"+
			"只说主人问的：是什么、把握多大（确定 / 看着像 / 不确定）；看不清就说要主人怎么做（拿近一点，或者把哪一面转向镜头）",
			brain.CameraAnswerChars)})}
)

// toolSchema 由参数结构体生成工具的 JSON Schema；descriptions 补上要引用常量、写不进结构体标签的字段说明。
func toolSchema[T any](descriptions map[string]string) json.RawMessage {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("生成工具参数的 Schema: %v", err))
	}
	for field, description := range descriptions {
		schema.Properties[field].Description = description
	}
	data, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("编码工具参数的 Schema: %v", err))
	}
	return data
}

// expertPrompt 是专家的提示词：只管看画面、交出结论（spoken 由代码直接念给主人）。
var expertPrompt = "你是看图专家，帮家里的语音助手看摄像头画面。主人一般是手举着东西、离镜头一臂远在问。" +
	"先看清形状、颜色、材质、上面的字和图案；字看不清就当看不清，不要猜字。再判断是什么，以及把握：确定、看着像、还是不确定。" +
	"画面里直接看到的比搜到的资料可靠，两者对不上时以画面为准，并说不确定。" +
	fmt.Sprintf("主人问的是画面上看不全的信息（规格、价格、用法、确切型号）时，用画面里读到的品牌、型号和外观特征去 %s 查，最多 %d 次；", searchTool, maxSearches) +
	"只问是什么、而画面已经看得出来时不用查。" +
	fmt.Sprintf("看完调用 %s 交出结论：spoken 由助手直接念给主人听，一两句，不超过 %d 个字，只说主人问的；details 写细节和依据，不念给主人。",
		reportTool, brain.CameraAnswerChars) +
	"助手调用你之前已经跟主人说过一句「我看看」之类的话，spoken 直接说结论，不用再垫话。" +
	"给了助手的性格时，spoken 用那个性格的口吻说，但只改措辞，不改判断，也不能超过字数。" +
	"用口语，不用列表和 Markdown。画面里没有的东西不要编。"

// reportReminder：专家说了判断却没调用 report_frame 时提醒它一次。
var reportReminder = fmt.Sprintf("用 %s 交出结论：spoken 一两句、不超过 %d 个字，details 写细节。", reportTool, brain.CameraAnswerChars)

// shortenRequest：交来的 spoken 超过字数上限时，让专家压短后再交一次（只看字数，不看内容）。
func shortenRequest(chars int) string {
	return fmt.Sprintf("spoken 有 %d 个字，超过了 %d 个字。意思不变，压短后再调用 %s 交一次。", chars, brain.CameraAnswerChars, reportTool)
}

// maxRounds：搜 maxSearches 次各一轮，交结论一轮，再各留一轮给提醒和压短。到上限还没交结论，就把它说的话当细节交给前台。
const maxRounds = maxSearches + 3

// ExamineFrame 看一帧画面，回答主人这一轮的话；需要时联网查（最多 maxSearches 次）。
// 结论经 report_frame 分两段交出；spoken 超过字数上限时让它压短一次，还超就照用（不截断）。
// 专家没按要求交时，它说的话放进 Details，由前台自己压短。
func (e *Expert) ExamineFrame(ctx context.Context, question brain.FrameQuestion) (brain.FrameReport, error) {
	ctx, cancel := context.WithTimeout(ctx, examineTimeout)
	defer cancel()
	started := time.Now()
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: expertPrompt}}
	if question.Style != "" {
		messages = append(messages, brain.ChatMessage{Role: brain.RoleUser, Text: "助手的性格（spoken 用这个口吻）：\n" + question.Style})
	}
	messages = append(messages,
		brain.ChatMessage{Role: brain.RoleUser, Text: "主人说：「" + question.Request + "」"},
		brain.ChatMessage{Role: brain.RoleUser, Text: "这是摄像头当前画面。", Image: question.Frame})
	searches, promptTokens, outputTokens := 0, 0, 0
	reminded := false
	var tooLong *brain.FrameReport // 超了字数、已经请它压短的那份
	finish := func(report brain.FrameReport) (brain.FrameReport, error) {
		e.logger.Info("看画面：专家看完", "model", e.modelName, "elapsed", since(started), "searches", searches,
			"spoken_chars", utf8.RuneCountInString(report.Spoken), "details_chars", utf8.RuneCountInString(report.Details),
			"prompt_tokens", promptTokens, "output_tokens", outputTokens)
		return report, nil
	}
	for round := 1; ; round++ {
		specs := []brain.ToolSpec{reportSpec}
		if e.search != nil && searches < maxSearches && round < maxRounds {
			specs = append(specs, searchSpec)
		}
		text, calls, usage, err := complete(ctx, e.model, messages, specs)
		promptTokens, outputTokens = promptTokens+usage.PromptTokens, outputTokens+usage.OutputTokens
		if err != nil {
			e.logger.Warn("看画面的专家出错", "model", e.modelName, "elapsed", since(started), "err", err)
			return brain.FrameReport{}, err
		}
		if report, ok := reportIn(calls); ok {
			chars := utf8.RuneCountInString(report.Spoken)
			if tooLong == nil && chars > brain.CameraAnswerChars && round < maxRounds {
				tooLong = &report
				e.logger.Info("看画面：专家那句超了字数，请它压短", "chars", chars)
				messages = e.answerCalls(ctx, messages, round, text, calls, &searches, shortenRequest(chars))
				continue
			}
			if tooLong != nil && report.Spoken == "" { // 压短时没交出那句：用压短前的
				return finish(*tooLong)
			}
			return finish(report)
		}
		if tooLong != nil { // 请它压短后没再交：用压短前的
			return finish(*tooLong)
		}
		text = strings.TrimSpace(text)
		if text == "" && len(calls) == 0 {
			return brain.FrameReport{}, errors.New("看画面的专家没有给出判断")
		}
		if round >= maxRounds || (len(calls) == 0 && reminded) {
			if text == "" {
				return brain.FrameReport{}, errors.New("看画面的专家没有给出判断")
			}
			return finish(brain.FrameReport{Details: text})
		}
		if len(calls) == 0 { // 说了判断但没交结论：提醒一次
			messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text},
				brain.ChatMessage{Role: brain.RoleUser, Text: reportReminder})
			reminded = true
			continue
		}
		messages = e.answerCalls(ctx, messages, round, text, calls, &searches, "")
	}
}

// answerCalls 把专家这一轮的工具调用和各自的结果接到消息后面：搜索照常执行；report_frame 的结果是 reportReply
// （请它压短时），没有时按没有这个工具处理。
func (e *Expert) answerCalls(ctx context.Context, messages []brain.ChatMessage, round int, text string, calls []brain.ToolCall,
	searches *int, reportReply string) []brain.ChatMessage {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("v%d-%d", round, i)
		}
	}
	messages = append(messages, brain.ChatMessage{Role: brain.RoleAssistant, Text: text, ToolCalls: calls})
	for _, call := range calls {
		result := reportReply
		if call.Name != reportTool || reportReply == "" {
			result = e.runTool(ctx, call, searches)
		}
		messages = append(messages, brain.ChatMessage{Role: brain.RoleTool, ToolName: call.Name, ToolCallID: call.ID, Text: result})
	}
	return messages
}

// reportIn 从这一轮的工具调用里取出 report_frame 交的结论；没有、或两段都空时 ok = false。
func reportIn(calls []brain.ToolCall) (brain.FrameReport, bool) {
	for _, call := range calls {
		if call.Name != reportTool {
			continue
		}
		var args reportArgs
		data, err := json.Marshal(call.Arguments)
		if err != nil || json.Unmarshal(data, &args) != nil {
			return brain.FrameReport{}, false
		}
		report := brain.FrameReport{Spoken: strings.TrimSpace(args.Spoken), Details: strings.TrimSpace(args.Details)}
		return report, report.Spoken != "" || report.Details != ""
	}
	return brain.FrameReport{}, false
}

// runTool 执行专家的一次工具调用；出错也作为文字交给专家，让它根据画面回答。
func (e *Expert) runTool(ctx context.Context, call brain.ToolCall, searches *int) string {
	if call.Name == reportTool { // 交来的参数不对或两段都空
		return "交来的结论是空的，或参数不对：按说明重新调用 " + reportTool + "。"
	}
	if call.Name != searchTool {
		return "没有叫 " + call.Name + " 的工具"
	}
	if *searches >= maxSearches {
		return fmt.Sprintf("搜索次数已经用完（上限 %d 次）。根据画面和已有资料回答。", maxSearches)
	}
	var args searchArgs
	if data, err := json.Marshal(call.Arguments); err == nil {
		_ = json.Unmarshal(data, &args)
	}
	query := args.Query
	if strings.TrimSpace(query) == "" {
		return "query 不能为空"
	}
	*searches++
	result, err := e.search(ctx, query)
	if err != nil {
		return "搜索失败：" + err.Error() + "。根据画面回答，并说没能核对。"
	}
	return result
}

// complete 请求模型一次，收齐文字、工具调用与用量。
func complete(ctx context.Context, model brain.ChatModel, messages []brain.ChatMessage, specs []brain.ToolSpec) (string, []brain.ToolCall, brain.ReplyUsage, error) {
	var text strings.Builder
	var calls []brain.ToolCall
	var usage brain.ReplyUsage
	for delta, err := range model.StreamReply(ctx, messages, specs) {
		if err != nil {
			return text.String(), calls, usage, err
		}
		text.WriteString(delta.Text)
		calls = append(calls, delta.ToolCalls...)
		if delta.Usage != nil {
			usage = *delta.Usage
		}
	}
	return text.String(), calls, usage, nil
}

func since(started time.Time) time.Duration { return time.Since(started).Round(time.Millisecond) }
