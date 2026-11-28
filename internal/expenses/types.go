// Package expenses 是记账（第 19 段）：拍小票 / 截图 → 模型看图解析成草稿（看不清的标出来）→ 主人确认（可以改任何一项）才入账；
// 账目按月、按分类、按店汇总，换成默认货币并附换算提示（第 14 段的规则）。和 haro 自己的花费账本（模型、搜索的费用）分开存、分开显示。
package expenses

import (
	"errors"
	"time"

	"github.com/bukahou/haro/internal/money"
)

// 小票的状态。
const (
	StatusParsing   = "parsing"   // 正在解析
	StatusReady     = "ready"     // 解析好了，等主人确认
	StatusFailed    = "failed"    // 解析失败（Failure 是原因）
	StatusConfirmed = "confirmed" // 主人确认了，已经入账
)

// 解析失败的原因（错误码，App 按它显示说明）。
const (
	FailureUnreadable  = "unreadable"  // 看不出是小票、或者看不清
	FailureUnavailable = "unavailable" // 模型用不了（连不上、额度用完）
	FailureInterrupted = "interrupted" // 服务端重启，没解析完
)

// 上限（定值）。
const (
	MaxItems         = 200
	MaxTextRunes     = 200 // 店名、名称、付款方式、分类
	MaxNoteRunes     = 1000
	parseConcurrency = 2 // 同时解析几张
	parseTimeout     = 2 * time.Minute
)

// 错误。
var (
	ErrNotFound = errors.New("没有这笔账 / 这张小票")
	ErrInvalid  = errors.New("账目不合规")
	ErrState    = errors.New("这张小票现在不能这样做（还在解析、已经入账、或者解析失败了）")
)

// Item 是一行明细。金额是原始币种；数量、税率可以没有（零值）。
type Item struct {
	Name      string
	Quantity  string // 十进制写法；空 = 没写
	UnitPrice money.Amount
	Amount    money.Amount
	TaxRate   string // 百分比的十进制写法（例如 "8"、"10"）；空 = 没写
}

// Draft 是解析出的（或者主人改过的）草稿：金额都是十进制写法，入账时再校验。Uncertain 是看不清、拿不准的字段名。
type Draft struct {
	Merchant      string      `json:"merchant"`
	SpentAt       *time.Time  `json:"spent_at,omitempty"`
	Currency      string      `json:"currency"`
	Items         []DraftItem `json:"items"`
	Subtotal      string      `json:"subtotal,omitempty"`
	Tax           string      `json:"tax,omitempty"`
	Total         string      `json:"total"`
	PaymentMethod string      `json:"payment_method,omitempty"`
	Category      string      `json:"category,omitempty"`
	Note          string      `json:"note,omitempty"`
	Uncertain     []string    `json:"uncertain,omitempty"`
}

// DraftItem 是草稿里的一行明细（十进制写法）。
type DraftItem struct {
	Name      string `json:"name"`
	Quantity  string `json:"quantity,omitempty"`
	UnitPrice string `json:"unit_price,omitempty"`
	Amount    string `json:"amount"`
	TaxRate   string `json:"tax_rate,omitempty"`
}

// Receipt 是交上来的一张小票。
type Receipt struct {
	ID, Person, AttachmentID string
	CategoryHint, NoteHint   string
	Status                   string
	Draft                    *Draft
	Failure                  string
	ExpenseID                string
	CreatedAt, UpdatedAt     time.Time
}

// Expense 是一笔账。
type Expense struct {
	ID, Person           string
	Merchant             string
	SpentAt              time.Time
	Total                money.Amount
	Subtotal, Tax        *money.Amount
	PaymentMethod        string
	Category, Note       string
	Items                []Item
	AttachmentID         string // 原图；手记的为空
	CreatedAt, UpdatedAt time.Time
}

// Category 是一个分类：固定的几类 Builtin 为 true（名字按语言给），主人加的名字照原样。
type Category struct {
	Key     string
	Name    string
	Builtin bool
}

// BuiltinCategories 是固定的几类（键只在这里定义一处；名字在 App 的文字里，每种语言一份）。
var BuiltinCategories = []string{"groceries", "dining", "daily", "transport", "housing", "utilities", "medical", "education",
	"entertainment", "shopping", "travel", "other"}
