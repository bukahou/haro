package expenses

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// saveTool 是代理交回草稿用的工具（只在这里定义一处；正文里用 {{.SaveTool}} 引用）。
const saveTool = "save_receipt"

// PromptValues 是代理「记账」正文里引用的值。
func PromptValues() map[string]any { return map[string]any{"SaveTool": saveTool} }

type receiptInput struct {
	Readable      bool          `json:"readable" jsonschema:"图里是不是一张看得清的小票、发票、收据或订单页；不是或看不清时为 false，别的都不用填"`
	Merchant      string        `json:"merchant,omitempty" jsonschema:"店名（订单页写卖家或平台）"`
	SpentAt       string        `json:"spent_at,omitempty" jsonschema:"消费的时间（RFC 3339，带时区偏移，按店所在地；只知道日期时写那天 12:00）"`
	Currency      string        `json:"currency,omitempty" jsonschema:"币种（ISO 4217，例如 JPY、CNY、USD）"`
	Items         []receiptItem `json:"items,omitempty" jsonschema:"每一样商品（按小票上的顺序）"`
	Subtotal      string        `json:"subtotal,omitempty" jsonschema:"小计（十进制数字，不带货币符号和千位分隔符）"`
	Tax           string        `json:"tax,omitempty" jsonschema:"税额（十进制数字）"`
	Total         string        `json:"total,omitempty" jsonschema:"合计、实付金额（十进制数字）"`
	PaymentMethod string        `json:"payment_method,omitempty" jsonschema:"付款方式（现金、信用卡、电子支付的名字等），小票上写了才填"`
	Category      string        `json:"category,omitempty" jsonschema:"从给的分类清单里选一个最合适的键；都不合适时不填"`
	Uncertain     []string      `json:"uncertain,omitempty" jsonschema:"看不清、拿不准的字段名（merchant、spent_at、currency、items、subtotal、tax、total、payment_method）"`
}

type receiptItem struct {
	Name      string `json:"name" jsonschema:"商品名（照小票上写的）"`
	Quantity  string `json:"quantity,omitempty" jsonschema:"数量（十进制数字），写了才填"`
	UnitPrice string `json:"unit_price,omitempty" jsonschema:"单价（十进制数字），写了才填"`
	Amount    string `json:"amount" jsonschema:"这一行的金额（十进制数字）"`
	TaxRate   string `json:"tax_rate,omitempty" jsonschema:"税率的百分比（例如 8、10），写了才填"`
}

var saveSpec = brain.ToolSpec{Name: saveTool, Parameters: brain.ParametersFor[receiptInput](),
	Description: "交回从图里读出的账目草稿。看完图一定要调用一次。"}

// Hint 是上传时主人给的：点的分类、写的一句说明。
type Hint struct{ Category, Note string }

// Parser 看图解析小票（AgentParser 实现）：交回草稿；不是小票、看不清时返回 ErrUnreadable。
type Parser interface {
	Parse(ctx context.Context, image []byte, categories []Category, hint Hint) (Draft, error)
}

// ErrUnreadable 是图里看不出是小票、或者看不清。
var ErrUnreadable = errors.New("看不出是小票、或者看不清")

// AgentParser 用代理「记账」解析。
type AgentParser struct{ Agent brain.Agent }

// Parse 实现 Parser：图交给代理，它用 save_receipt 交回草稿；分类只认清单里的键。
func (p AgentParser) Parse(ctx context.Context, image []byte, categories []Category, hint Hint) (Draft, error) {
	keys := make([]string, 0, len(categories))
	shown := make([]string, 0, len(categories))
	for _, category := range categories {
		keys = append(keys, category.Key)
		if category.Builtin {
			shown = append(shown, category.Key)
		} else {
			shown = append(shown, category.Key+"（"+category.Name+"）")
		}
	}
	var request strings.Builder
	request.WriteString("可选的分类（键）：" + strings.Join(shown, "、") + "\n")
	if hint.Category != "" {
		request.WriteString("主人点的分类：" + hint.Category + "\n")
	}
	if hint.Note != "" {
		request.WriteString("主人写的说明：" + hint.Note + "\n")
	}
	messages := []brain.ChatMessage{{Role: brain.RoleSystem, Text: p.Agent.Instructions},
		{Role: brain.RoleUser, Text: request.String(), Image: image}}
	reply, err := brain.CollectReply(ctx, p.Agent.Model, brain.ChatRequest{Messages: messages, Tools: []brain.ToolSpec{saveSpec}})
	if err != nil {
		return Draft{}, err
	}
	for _, call := range reply.ToolCalls {
		if call.Name != saveTool { // 执行方自己守门：只认给了它的工具
			continue
		}
		input, err := brain.DecodeArguments[receiptInput](call.Arguments)
		if err != nil {
			return Draft{}, fmt.Errorf("代理交回的草稿不对: %w", err)
		}
		if !input.Readable {
			return Draft{}, ErrUnreadable
		}
		return draftOf(input, keys, hint), nil
	}
	return Draft{}, errors.New("代理没有交回草稿（没调用 " + saveTool + "）")
}

// draftOf 把代理交回的整理成草稿：分类不在清单里的不要（用主人点的）；时间解不开的当成拿不准。
func draftOf(input receiptInput, categories []string, hint Hint) Draft {
	draft := Draft{Merchant: input.Merchant, Currency: strings.ToUpper(strings.TrimSpace(input.Currency)), Subtotal: input.Subtotal,
		Tax: input.Tax, Total: input.Total, PaymentMethod: input.PaymentMethod, Note: hint.Note, Uncertain: input.Uncertain}
	if spent, err := time.Parse(time.RFC3339, input.SpentAt); err == nil {
		spent = spent.UTC()
		draft.SpentAt = &spent
	} else if !slices.Contains(draft.Uncertain, "spent_at") {
		draft.Uncertain = append(draft.Uncertain, "spent_at")
	}
	switch {
	case hint.Category != "":
		draft.Category = hint.Category
	case slices.Contains(categories, input.Category):
		draft.Category = input.Category
	}
	for _, item := range input.Items {
		draft.Items = append(draft.Items, DraftItem{Name: item.Name, Quantity: item.Quantity, UnitPrice: item.UnitPrice, Amount: item.Amount,
			TaxRate: item.TaxRate})
	}
	return draft
}
