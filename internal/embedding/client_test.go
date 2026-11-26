package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeService 是 OpenAI 兼容的嵌入接口的替身：记下收到的输入；每条输入回一个按长度算的向量，倒着排（编号照给），
// badDim 不为 0 时最后一条回这么多维（模拟维度不一致）。
type fakeService struct {
	inputs []string
	badDim int
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/embeddings" {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	f.inputs = append(f.inputs, request.Input...)
	type item struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	var data []item
	for i := len(request.Input) - 1; i >= 0; i-- {
		vector := []float64{float64(len([]rune(request.Input[i]))), 1, 0}
		if f.badDim > 0 && i == len(request.Input)-1 {
			vector = make([]float64, f.badDim)
		}
		data = append(data, item{Object: "embedding", Index: i, Embedding: vector})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": request.Model, "data": data,
		"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
}

func newTestClient(t *testing.T, service http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	return NewClient(Options{BaseURL: server.URL + "/v1", Model: "intfloat/multilingual-e5-small",
		QueryPrefix: "query: ", DocumentPrefix: "passage: "})
}

// 问句、内容各加各的前缀；回复按编号对回原来的顺序（服务端打乱了也一样）。
func TestEmbedPrefixesAndOrder(t *testing.T) {
	service := &fakeService{}
	client := newTestClient(t, service)
	vectors, err := client.EmbedDocuments(context.Background(), []string{"不吃香菜", "车停在 C3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || vectors[0][0] != float32(len([]rune("passage: 不吃香菜"))) || vectors[1][0] != float32(len([]rune("passage: 车停在 C3"))) {
		t.Errorf("向量应按原来的顺序：%v", vectors)
	}
	query, err := client.EmbedQuery(context.Background(), "忌口")
	if err != nil || len(query) != 3 {
		t.Fatalf("问句：%v %v", query, err)
	}
	if want := []string{"passage: 不吃香菜", "passage: 车停在 C3", "query: 忌口"}; strings.Join(service.inputs, "|") != strings.Join(want, "|") {
		t.Errorf("前缀：%q", service.inputs)
	}
	if client.Model() != "intfloat/multilingual-e5-small" {
		t.Errorf("模型名：%q", client.Model())
	}
}

// 回复和请求对不上（维度不一致）时报错，不把坏向量交出去。
func TestEmbedRejectsInconsistentDimensions(t *testing.T) {
	client := newTestClient(t, &fakeService{badDim: 5})
	if _, err := client.EmbedDocuments(context.Background(), []string{"一", "二"}); !errors.Is(err, errEmbeddingResponse) {
		t.Errorf("维度不一致应报错：%v", err)
	}
}

// 服务出错（例如没起来）时报错，带上是哪个服务。
func TestEmbedReportsServiceError(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "loading model", http.StatusServiceUnavailable)
	}))
	if _, err := client.EmbedQuery(context.Background(), "你好"); err == nil || !strings.Contains(err.Error(), "嵌入服务") {
		t.Errorf("应报错并写明是嵌入服务：%v", err)
	}
}

func TestVectorRoundTrip(t *testing.T) {
	vector := []float32{0.5, -1.25, 3}
	decoded, err := DecodeVector(EncodeVector(vector))
	if err != nil || len(decoded) != 3 || decoded[0] != 0.5 || decoded[1] != -1.25 || decoded[2] != 3 {
		t.Fatalf("往返：%v %v", decoded, err)
	}
	if _, err := DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Error("字节数不是 4 的倍数应报错")
	}
}
