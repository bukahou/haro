package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// pageReader 是读网页的替身：记下读了哪些网址、问的 focus；errorFor 里的网址读不到。
type pageReader struct {
	read     []string
	focus    string
	errorFor map[string]string
}

func (p *pageReader) Search(context.Context, string, int, SearchRecorder) ([]SearchResult, error) {
	return nil, nil
}
func (p *pageReader) ReadPages(_ context.Context, urls []string, maxRunes int, focus string, _ SearchRecorder) ([]PageContent, error) {
	p.read, p.focus = append(p.read, urls...), focus
	var pages []PageContent
	for _, url := range urls {
		if reason, ok := p.errorFor[url]; ok {
			pages = append(pages, PageContent{URL: url, Error: reason})
			continue
		}
		pages = append(pages, PageContent{URL: url, Content: strings.Repeat("字", min(maxRunes, 10))})
	}
	return pages, nil
}
func (p *pageReader) SearchCredits() int        { return 1 }
func (p *pageReader) ReadCredits(pages int) int { return (pages + 4) / 5 }

// 读网页：只收 http / https 的网址、一次最多 3 页（多给的、不是网址的、读不到的都写进 not_read，带原因），focus 交给读的那一层；
// 先查额度（和搜索同一个上限），额度用完时不读。
func TestReadWebPages(t *testing.T) {
	ctx := context.Background()
	reader := &pageReader{errorFor: map[string]string{"https://c.example.com/": "网站拒绝程序访问"}}
	meter := &countingMeter{}
	box, err := NewToolbox(ctx, quietLogger, WebSearchTools(reader, meter, quietLogger))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	urls := []any{"https://a.example.com/timetable", "ftp://b.example.com/", "https://c.example.com/", "http://d.example.com/", "https://e.example.com/"}
	out, err := callJSON[readWebPagesOutput](t, box, ReadWebPagesTool, map[string]any{"urls": urls, "focus": "21 時台の発車時刻"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://a.example.com/timetable", "https://c.example.com/", "http://d.example.com/"}; !slices.Equal(reader.read, want) {
		t.Errorf("应只读前 3 个合法的网址：%v", reader.read)
	}
	if reader.focus != "21 時台の発車時刻" {
		t.Errorf("focus 应交给读的那一层：%q", reader.focus)
	}
	if len(out.Pages) != 2 || len(out.NotRead) != 3 {
		t.Fatalf("读到 2 页，3 个没读（不是网址、超过 3 页、读不到）：%+v", out)
	}
	reasons := map[string]string{}
	for _, page := range out.NotRead {
		reasons[page.URL] = page.Reason
	}
	if !strings.Contains(reasons["ftp://b.example.com/"], "网址") || !strings.Contains(reasons["https://e.example.com/"], "最多") ||
		reasons["https://c.example.com/"] != "网站拒绝程序访问" {
		t.Errorf("没读的要写原因：%v", reasons)
	}

	meter.refuse(errors.New("今天的搜索额度用完了"))
	reader.read = nil
	if _, err := callJSON[readWebPagesOutput](t, box, ReadWebPagesTool, map[string]any{"urls": []any{"https://a.example.com/"}}); err == nil {
		t.Error("额度用完时应报错")
	}
	if len(reader.read) != 0 {
		t.Errorf("额度用完时不读：%v", reader.read)
	}
	if _, err := callJSON[readWebPagesOutput](t, box, ReadWebPagesTool, map[string]any{"urls": []any{}}); err == nil {
		t.Error("没给网址应报错")
	}
}
