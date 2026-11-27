//go:build eval

package research

import (
	"context"
	"log/slog"
	"time"
)

// 只在评测（eval 标签）时编进来：让评测直接考审查员。给它一份草稿和草稿列出的来源读到的内容，走线上审查的同一段代码
// （摘录的取法、交给审查的内容、submit_review 的定义和核对都一样），不经过调查。

// ExamPageRunes 是读来源时每页最多多少字：和调查时一样，审查看到的摘录就和线上一样长。
const ExamPageRunes = pageRunes

// ExamDoubt 是审查交回的一条疑点。
type ExamDoubt struct {
	Claim    string `json:"claim"`
	Kind     string `json:"kind"`
	Evidence string `json:"evidence"`
}

// ExamVerdict 是审查交回的结果；Tokens 是这次审查的用量。
type ExamVerdict struct {
	Pass   bool        `json:"pass"`
	Doubts []ExamDoubt `json:"doubts,omitempty"`
	Tokens int         `json:"tokens"`
}

// ExamReview 请 agents 里的审查员审一份草稿：sources 是草稿列出的来源（顺序照草稿），pages 是这些来源读到的内容。
// 审查没做成（出错、超时、没用工具交回、交回的不对）时返回错误，Tokens 照样记着。
func ExamReview(ctx context.Context, agents Agents, now time.Time, question, draft string, sources []string,
	pages []Page) (ExamVerdict, error) {
	runner, err := NewRunner(nil, agents, nil, nil, examBudget{}, DefaultLimits, slog.New(slog.DiscardHandler))
	if err != nil {
		return ExamVerdict{}, err
	}
	notes := sourceNotes{}
	notes.addPages(pages)
	verdict, tokens, err := runner.review(ctx, question, conclusionDraft{Conclusion: draft, Sources: sources}, notes, now)
	result := ExamVerdict{Pass: verdict.Verdict == verdictPass, Tokens: tokens}
	if err != nil {
		return result, err
	}
	for _, doubt := range verdict.Doubts {
		result.Doubts = append(result.Doubts, ExamDoubt{Claim: doubt.Claim, Kind: string(doubt.Kind), Evidence: doubt.Evidence})
	}
	return result, nil
}

// examBudget 不限额度：评测自己按美元记账、到上限就停。
type examBudget struct{}

func (examBudget) AllowResearch(context.Context) error { return nil }
