//go:build eval

// 按意思找的小评测：固定的几条记住的事和一组问句（中日英，含跨语言的问法），用真的嵌入服务算向量，看最像的前 1 名、前 3 名里
// 有没有该找到的那一条。用来比较嵌入模型、确认换模型以后没有变差。嵌入服务在本机或集群里，不花钱；结果只打印，不进 CI。
//
//	HARO_EVAL_EMBEDDING_URL=http://127.0.0.1:8080/v1 [HARO_EVAL_EMBEDDING_MODEL=…] \
//	  go test -tags eval -run RecallEval -v -count=1 ./internal/memory/
package memory

import (
	"cmp"
	"context"
	"os"
	"testing"

	"github.com/bukahou/haro/internal/embedding"
)

// evalMemories 是记住的事（编号 = 下标 + 1）：都是编出来的生活细节，不是真实的人。
var evalMemories = []string{
	"不吃香菜",
	"车停在 B2 层的 C3 区 7 号位",
	"每周三晚上去健身房",
	"平时喝无糖的乌龙茶",
	"女儿的生日是 4 月 12 日",
	"钥匙放在玄关的抽屉里",
	"英語の資料は共有フォルダの「2026」に置いてある",
	"The Wi-Fi router is in the living room closet",
	"常去的超市是车站旁边那家",
	"周五晚上一般在家看电影",
}

// evalQueries 是问句和应该找到的那一条（编号）。
var evalQueries = []struct {
	query string
	want  int
}{
	{"我有什么不吃的？", 1},
	{"我的车停哪了", 2},
	{"駐車した場所はどこ？", 2},
	{"我一般周几去健身", 3},
	{"我平时喝什么饮料", 4},
	{"女儿哪天过生日", 5},
	{"where are my keys", 6},
	{"钥匙在哪", 6},
	{"英文的资料放在哪里", 7},
	{"路由器在哪", 8},
	{"Where is the router?", 8},
	{"我平时去哪个超市", 9},
	{"周末前一天晚上我一般干什么", 10},
}

func TestRecallEval(t *testing.T) {
	url := os.Getenv("HARO_EVAL_EMBEDDING_URL")
	if url == "" {
		t.Skip("没设 HARO_EVAL_EMBEDDING_URL：跳过（要真的嵌入服务）")
	}
	ctx := context.Background()
	client := embedding.NewClient(embedding.Options{BaseURL: url,
		Model:       cmp.Or(os.Getenv("HARO_EVAL_EMBEDDING_MODEL"), "intfloat/multilingual-e5-small"),
		QueryPrefix: "query: ", DocumentPrefix: "passage: "})
	vectors, err := client.EmbedDocuments(ctx, evalMemories)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndex()
	for i, vector := range vectors {
		index.Put(KindNote, "主人", int64(i+1), vector)
	}
	top1, top3 := 0, 0
	for _, q := range evalQueries {
		vector, err := client.EmbedQuery(ctx, q.query)
		if err != nil {
			t.Fatal(err)
		}
		hits := index.Search(vector, KindNote, []string{"主人"}, 3)
		rank := 0
		for i, hit := range hits {
			if hit.ID == int64(q.want) {
				rank = i + 1
			}
		}
		switch rank {
		case 1:
			top1++
			top3++
		case 2, 3:
			top3++
		}
		got := make([]int64, len(hits))
		for i, hit := range hits {
			got[i] = hit.ID
		}
		t.Logf("%-28s 应找到 #%d，前 3 名 %v（第 %d 名）", q.query, q.want, got, rank)
	}
	t.Logf("模型 %s：前 1 名命中 %d/%d，前 3 名命中 %d/%d", client.Model(), top1, len(evalQueries), top3, len(evalQueries))
}
