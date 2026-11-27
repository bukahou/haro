package spending

import "github.com/bukahou/haro/internal/money"

// jpy 是测试里的日元金额。
func jpy(value float64) money.Amount { return money.Of(value, "JPY") }

// spentJPY 是合计按 fixedRates 换成日元的数（账本里有日元，也有按原始币种记的美元）。
func spentJPY(totals Totals) float64 {
	total, err := fixedRates{}.Convert(totals.Spent, "JPY")
	if err != nil {
		panic(err)
	}
	return total.Amount.Value()
}

// fixedRates 是测试用的汇率：1 美元 = 100 日元。
type fixedRates struct{}

func (fixedRates) Convert(sum money.Sum, to string) (money.Total, error) {
	perUSD := map[string]float64{"USD": 1, "JPY": 100}
	total := money.Total{Amount: money.Amount{Currency: to}}
	for _, code := range sum.Currencies() {
		rateFrom, okFrom := perUSD[code]
		rateTo, okTo := perUSD[to]
		if !okFrom || !okTo {
			return money.Total{}, money.ErrNoRate
		}
		total.Amount.Micros += int64(float64(sum[code]) / rateFrom * rateTo)
		if code != to {
			total.Converted = append(total.Converted, code)
		}
	}
	return total, nil
}
