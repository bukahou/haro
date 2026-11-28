// Package money 是金额：数值 + 币种（ISO 4217）。
//
// 数值存成「百万分之一个货币单位」的整数（micros，和 Google Ads 等接口同一种写法）：模型调用一次常常只有零点零几美分，
// 用浮点累加会有误差。每笔按原始币种记；要合计成一种货币时经 Converter 按汇率换算，并说明换算了哪些、用的哪天的汇率。
package money

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/currency"
)

// MicrosPerUnit 是一个货币单位有多少 micros。
const MicrosPerUnit = 1_000_000

// Amount 是一笔金额。零值是「没有币种的 0」，只用来表示「还没有」。
type Amount struct {
	Micros   int64
	Currency string // ISO 4217 的大写代码，例如 USD、JPY、CNY
}

// Of 把一个数（例如 0.0123 美元）变成金额，四舍五入到 micros。币种照原样（调用方保证是规范的代码，见 ParseCurrency）。
func Of(value float64, code string) Amount {
	return Amount{Micros: int64(math.Round(value * MicrosPerUnit)), Currency: code}
}

// Value 是金额的数值（例如 0.0123）。只用来比较大小、显示，⛔ 不拿来累加。
func (a Amount) Value() float64 { return float64(a.Micros) / MicrosPerUnit }

// IsZero 判断数值是不是 0。
func (a Amount) IsZero() bool { return a.Micros == 0 }

// Rounded 是按这种货币的最小单位四舍五入后的十进制写法（JPY 没有小数、USD 和 CNY 两位）：给人看、给接口用。
func (a Amount) Rounded() string {
	scale := 2
	if unit, err := currency.ParseISO(a.Currency); err == nil {
		scale, _ = currency.Standard.Rounding(unit)
	}
	return strconv.FormatFloat(a.Value(), 'f', scale, 64)
}

// String 是「数值 币种」（日志、交给模型的说明里用），例如「120 JPY」。
func (a Amount) String() string { return a.Rounded() + " " + a.Currency }

// ParseCurrency 检查币种代码（ISO 4217，大小写都行），返回规范的大写写法。
func ParseCurrency(code string) (string, error) {
	unit, err := currency.ParseISO(code)
	if err != nil {
		return "", fmt.Errorf("不认识的币种代码 %q（要 ISO 4217 的三个字母，例如 JPY、USD、CNY）", code)
	}
	return unit.String(), nil
}

// Sum 是按币种分开的合计（币种 → micros）。不同币种不直接相加：要合成一种时用 Converter。
type Sum map[string]int64

// Add 加上一笔（0 也会留下这个币种）。
func (s Sum) Add(amount Amount) { s[amount.Currency] += amount.Micros }

// AddSum 加上另一份合计。
func (s Sum) AddSum(other Sum) {
	for code, micros := range other {
		s[code] += micros
	}
}

// Currencies 是有哪些币种（按代码排序）。
func (s Sum) Currencies() []string { return slices.Sorted(maps.Keys(s)) }

// Amount 是其中一种币种的合计（没有这种币种时是 0）。
func (s Sum) Amount(code string) Amount { return Amount{Micros: s[code], Currency: code} }

// Total 是换成一种货币的合计，带换算的说明（汇总时给人看的提示用）。
type Total struct {
	Amount    Amount
	Converted []string  // 从哪些币种换算过来的（按代码排序；都是同一种币种时为空）
	RateDate  time.Time // 用的哪天的汇率（没换算时为零值）
	Missing   []string  // 没有汇率、没算进合计的币种（只有 ConvertWhatCan 会给；按代码排序）
}

// ConvertWhatCan 把能换的都换成 to：没有汇率的币种不算进合计、列在 Missing 里。给汇总主人账目这类
// 不该因为一种币种就整个算不出来的地方用（花费上限这类要算准的用 Converter.Convert，换不了就报错）。
func ConvertWhatCan(converter Converter, sum Sum, to string) (Total, error) {
	total, err := converter.Convert(sum, to)
	if !errors.Is(err, ErrNoRate) {
		return total, err
	}
	combined := Total{Amount: Amount{Currency: to}}
	for _, code := range sum.Currencies() {
		part, err := converter.Convert(Sum{code: sum[code]}, to)
		switch {
		case errors.Is(err, ErrNoRate):
			combined.Missing = append(combined.Missing, code)
			continue
		case err != nil:
			return Total{}, err
		}
		combined.Amount.Micros += part.Amount.Micros
		combined.Converted = append(combined.Converted, part.Converted...)
		if part.RateDate.After(combined.RateDate) {
			combined.RateDate = part.RateDate
		}
	}
	return combined, nil
}

// ErrNoRate 表示换算不了：没有这种币种的汇率。
var ErrNoRate = errors.New("没有这种币种的汇率")

// Converter 把按币种分开的合计换成一种货币。实现见 ECBRates；同一种币种时不需要汇率。
type Converter interface {
	Convert(sum Sum, to string) (Total, error)
}

// SameCurrencyOnly 是不换算的 Converter：合计里只有目标币种时直接给出，有别的币种就报 ErrNoRate。
// 给只用一种货币的部署（例如默认货币就是美元）、和测试用。
type SameCurrencyOnly struct{}

// Convert 实现 Converter。
func (SameCurrencyOnly) Convert(sum Sum, to string) (Total, error) {
	for code, micros := range sum {
		if code != to && micros != 0 {
			return Total{}, fmt.Errorf("%w：要把 %s 换成 %s", ErrNoRate, code, to)
		}
	}
	return Total{Amount: sum.Amount(to)}, nil
}

// USD 是美元的代码（服务商按美元收费：模型、搜索的单价都是美元）。
const USD = "USD"

// String 是给人看的写法：各币种按代码排序、用「 + 」连起来，例如「7 JPY + 0.03 USD」；没有时是「0」。
func (s Sum) String() string {
	var parts []string
	for _, code := range s.Currencies() {
		parts = append(parts, s.Amount(code).String())
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, " + ")
}

// ParseAmount 把十进制写法（例如 "1234.5"、"-80"）精确地变成金额（不经过浮点）；超过 micros 精度的小数报错。币种按 ParseCurrency 校验。
func ParseAmount(text, code string) (Amount, error) {
	normalized, err := ParseCurrency(code)
	if err != nil {
		return Amount{}, err
	}
	value, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok || strings.ContainsAny(text, "/eE") {
		return Amount{}, fmt.Errorf("金额 %q 不是十进制数", text)
	}
	micros := new(big.Rat).Mul(value, new(big.Rat).SetInt64(MicrosPerUnit))
	if !micros.IsInt() || !micros.Num().IsInt64() {
		return Amount{}, fmt.Errorf("金额 %q 的小数太多或数太大", text)
	}
	return Amount{Micros: micros.Num().Int64(), Currency: normalized}, nil
}
