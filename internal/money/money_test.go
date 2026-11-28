package money

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// 金额：按 micros 存；四舍五入到这种货币的最小单位（日元没有小数，美元、人民币两位）。
func TestAmountRounding(t *testing.T) {
	for _, c := range []struct {
		amount Amount
		want   string
	}{
		{Of(120.4, "JPY"), "120"},
		{Of(0.0123, "USD"), "0.01"},
		{Of(12.345, "CNY"), "12.35"},
		{Of(3000, "JPY"), "3000"},
	} {
		if got := c.amount.Rounded(); got != c.want {
			t.Errorf("%+v：%q，应是 %q", c.amount, got, c.want)
		}
	}
	if Of(0.000_001, "USD").Micros != 1 {
		t.Error("百万分之一美元应是 1 micro")
	}
	if code, err := ParseCurrency("jpy"); err != nil || code != "JPY" {
		t.Errorf("小写也认：%q %v", code, err)
	}
	if _, err := ParseCurrency("YEN"); err == nil {
		t.Error("不是 ISO 4217 的代码应报错")
	}
}

// 合计按币种分开；同一种币种不用汇率，有别的币种时 SameCurrencyOnly 报错。
func TestSumAndSameCurrency(t *testing.T) {
	sum := Sum{}
	sum.Add(Of(100, "JPY"))
	sum.Add(Of(20, "JPY"))
	if total, err := (SameCurrencyOnly{}).Convert(sum, "JPY"); err != nil || total.Amount.Rounded() != "120" || len(total.Converted) != 0 {
		t.Errorf("同一种币种直接合计：%+v %v", total, err)
	}
	sum.Add(Of(1, "USD"))
	if _, err := (SameCurrencyOnly{}).Convert(sum, "JPY"); !errors.Is(err, ErrNoRate) {
		t.Errorf("有别的币种时应报 ErrNoRate：%v", err)
	}
	if !slices.Equal(sum.Currencies(), []string{"JPY", "USD"}) {
		t.Errorf("币种：%v", sum.Currencies())
	}
}

const ecbSample = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<gesmes:subject>Reference rates</gesmes:subject>
	<Cube>
		<Cube time='2026-11-27'>
			<Cube currency='USD' rate='1.1'/>
			<Cube currency='JPY' rate='176'/>
			<Cube currency='CNY' rate='7.7'/>
		</Cube>
	</Cube>
</gesmes:Envelope>`

func ecbServer(t *testing.T, healthy *atomic.Bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(ecbSample))
	}))
	t.Cleanup(server.Close)
	return server
}

// 拉到欧洲央行的参考汇率后按欧元交叉换算：1 美元 = 176 / 1.1 = 160 日元；换算过的币种和日期写在合计里。
func TestECBConvert(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server := ecbServer(t, &healthy)
	rates := NewECBRates(server.URL, "", server.Client(), quietLogger)
	if err := rates.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	sum := Sum{}
	sum.Add(Of(2, "USD"))
	sum.Add(Of(100, "JPY"))
	total, err := rates.Convert(sum, "JPY")
	if err != nil || total.Amount.Rounded() != "420" || !slices.Equal(total.Converted, []string{"USD"}) ||
		total.RateDate.Format("2006-01-02") != "2026-11-27" {
		t.Errorf("2 美元 + 100 日元 = 420 日元，换算的是美元：%+v %v", total, err)
	}
	cny := Sum{}
	cny.Add(Of(77, "CNY"))
	if total, err := rates.Convert(cny, "JPY"); err != nil || total.Amount.Rounded() != "1760" {
		t.Errorf("77 元 = 1760 日元：%+v %v", total, err)
	}
	unknown := Sum{}
	unknown.Add(Of(1, "BRL"))
	if _, err := rates.Convert(unknown, "JPY"); !errors.Is(err, ErrNoRate) {
		t.Errorf("没有汇率的币种应报 ErrNoRate：%v", err)
	}
}

// 拉不到时用上次存的；都没有就起不来（不按猜的汇率判断上限）。
func TestECBFallsBackToSavedCopy(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	server := ecbServer(t, &healthy)
	cache := filepath.Join(t.TempDir(), "data", "rates.json")
	if err := NewECBRates(server.URL, cache, server.Client(), quietLogger).Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	healthy.Store(false)
	rates := NewECBRates(server.URL, cache, server.Client(), quietLogger)
	if err := rates.Load(context.Background()); err != nil || rates.Table().PerEUR["JPY"] != 176 {
		t.Errorf("拉不到时应用存的那份：%v", err)
	}
	if err := NewECBRates(server.URL, "", server.Client(), quietLogger).Load(context.Background()); err == nil {
		t.Error("拉不到、也没有存的那份：应起不来")
	}
}

// 认不出的格式报错，不把半份数据当汇率用。
func TestParseECBRejectsGarbage(t *testing.T) {
	for _, body := range []string{"<html>maintenance</html>", "<Envelope><Cube></Cube></Envelope>",
		"<Envelope><Cube><Cube time='2026-11-27'><Cube currency='XX' rate='1'/></Cube></Cube></Envelope>"} {
		if _, err := parseECB(strings.NewReader(body)); err == nil {
			t.Errorf("应报错：%s", body)
		}
	}
}
