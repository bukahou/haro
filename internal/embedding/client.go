// Package embedding 把文字变成向量（长期记忆按意思检索用，v10 D4）：照 OpenAI 的嵌入接口（POST {base}/embeddings），
// 用官方 openai-go。服务可以是集群里的 Text Embeddings Inference（现在用的）、Ollama 的兼容端点或云端，换的时候只改配置。
//
// 有的模型要求问句和被检索的内容加不同的前缀（e5 系列：「query: 」「passage: 」）：前缀是配置项（模型特有的知识留在配置），
// 不写死在代码里。
package embedding

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Options 是嵌入服务的设置。
type Options struct {
	BaseURL string // OpenAI 兼容接口的根（例如 http://host/v1）；请求发到它下面的 embeddings
	Model   string // 模型名（存向量时一并记下：换了模型，旧向量要重算）
	APIKey  string // 自己部署的服务一般不用；空也照设（盖掉 SDK 从环境变量读到的别处的 key）
	// QueryPrefix、DocumentPrefix：问句、被检索的内容前面加的字（e5 系列要求；不要求的模型留空）
	QueryPrefix, DocumentPrefix string
	Timeout                     time.Duration // 一次请求最多等多久；零值 = DefaultTimeout
}

// DefaultTimeout：一次嵌入请求最多等这么久。定值：服务在集群里，一批十几条短句在 CPU 上一般不到一秒。
const DefaultTimeout = 10 * time.Second

// Client 调嵌入服务。可以并发使用。
type Client struct {
	client  openai.Client
	opts    Options
	timeout time.Duration
}

// NewClient 创建客户端（不连服务：第一次嵌入时才发请求）。
func NewClient(opts Options) *Client {
	base := opts.BaseURL
	if !strings.HasSuffix(base, "/") {
		base += "/" // SDK 把路径接在根后面：根要以 / 结尾
	}
	return &Client{opts: opts, timeout: cmp.Or(opts.Timeout, DefaultTimeout),
		client: openai.NewClient(option.WithBaseURL(base), option.WithAPIKey(opts.APIKey), option.WithMaxRetries(1))}
}

// Model 是用的模型名。
func (c *Client) Model() string { return c.opts.Model }

// EmbedQuery 把一句问句变成向量。
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.embed(ctx, c.opts.QueryPrefix, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// EmbedDocuments 把几段被检索的内容变成向量（顺序和 texts 一致）。一次多少条由调用方定。
func (c *Client) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return c.embed(ctx, c.opts.DocumentPrefix, texts)
}

// errEmbeddingResponse：嵌入服务的回复和请求对不上（条数、编号、维度）。
var errEmbeddingResponse = errors.New("嵌入服务的回复和请求对不上")

func (c *Client) embed(ctx context.Context, prefix string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	inputs := make([]string, len(texts))
	for i, text := range texts {
		inputs[i] = prefix + text
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.client.Embeddings.New(ctx, openai.EmbeddingNewParams{Model: c.opts.Model,
		Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: inputs},
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat}) // 写明要浮点数（各家的默认值不一定一样）
	if err != nil {
		return nil, fmt.Errorf("嵌入服务（%s）: %w", c.opts.BaseURL, err)
	}
	if len(response.Data) != len(texts) {
		return nil, fmt.Errorf("%w：送了 %d 条，回了 %d 条", errEmbeddingResponse, len(texts), len(response.Data))
	}
	vectors := make([][]float32, len(texts))
	dimensions := 0
	for _, item := range response.Data {
		index := int(item.Index)
		if index < 0 || index >= len(texts) || vectors[index] != nil {
			return nil, fmt.Errorf("%w：编号 %d 不对", errEmbeddingResponse, item.Index)
		}
		if dimensions == 0 {
			dimensions = len(item.Embedding)
		}
		if len(item.Embedding) == 0 || len(item.Embedding) != dimensions {
			return nil, fmt.Errorf("%w：向量的维度不一致（%d 和 %d）", errEmbeddingResponse, dimensions, len(item.Embedding))
		}
		vector := make([]float32, len(item.Embedding))
		for i, value := range item.Embedding {
			vector[i] = float32(value)
		}
		vectors[index] = vector
	}
	return vectors, nil
}
