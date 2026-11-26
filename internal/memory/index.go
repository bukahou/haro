package memory

import (
	"math"
	"slices"
	"sync"
)

// Index 是内存里的向量索引：记住的事和以前的对话各一类，按人过滤，按余弦相似度取前几名。
//
// 逐条算（2026-11-26 用户裁定，代替 sqlite-vec）：家里的规模（对话一年约两万轮）一次几毫秒，不加依赖、不改构建；
// 向量的真身在 SQLite 里，启动时载入。⚠️ 换掉的条件写在 docs：对话超过约 10 万轮，或一次检索超过 50 ms → 换 pgvector
// （只换这个类型后面的实现，Keeper 不变）。可以并发使用。
type Index struct {
	mu      sync.RWMutex
	entries map[entryKey][]float32 // 归一化过的向量（点积就是余弦相似度）
}

// Kind 是被检索的东西是哪一类。
type Kind int

const (
	KindNote Kind = iota // 记住的事（记事库里的一条）
	KindTurn             // 以前的对话（完整记录里的一轮）
)

// entryKey 指一条：哪一类、属于谁（记事：空 = 全家共用；对话：说话的人）、编号（记事的 id，对话的轮次）。
type entryKey struct {
	kind   Kind
	person string
	id     int64
}

// Hit 是检索到的一条和它的相似度（-1–1，越大越像）。
type Hit struct {
	Kind   Kind
	Person string
	ID     int64
	Score  float32
}

// NewIndex 建一个空索引。
func NewIndex() *Index { return &Index{entries: map[entryKey][]float32{}} }

// Put 放进（或替换）一条。零向量不放（没法比）。
func (x *Index) Put(kind Kind, person string, id int64, vector []float32) {
	normalized := normalize(vector)
	if normalized == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.entries[entryKey{kind, person, id}] = normalized
}

// Remove 去掉一条（例如忘掉的记事）。
func (x *Index) Remove(kind Kind, person string, id int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.entries, entryKey{kind, person, id})
}

// Len 是一共有几条。
func (x *Index) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.entries)
}

// Search 在这一类里、属于 people 里任一个人的条目中，找和 query 最像的 limit 条（最像的在前）。
// 维度和 query 不一样的（换模型之前算的）跳过。
func (x *Index) Search(query []float32, kind Kind, people []string, limit int) []Hit {
	normalized := normalize(query)
	if normalized == nil || limit <= 0 {
		return nil
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	var hits []Hit
	for key, vector := range x.entries {
		if key.kind != kind || !slices.Contains(people, key.person) || len(vector) != len(normalized) {
			continue
		}
		var score float32
		for i, value := range vector {
			score += value * normalized[i]
		}
		hits = append(hits, Hit{Kind: kind, Person: key.person, ID: key.id, Score: score})
	}
	slices.SortFunc(hits, func(a, b Hit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return int(a.ID - b.ID) // 一样像时编号小的在前（结果稳定）
	})
	return hits[:min(limit, len(hits))]
}

// normalize 把向量缩放到长度 1（复制一份）；零向量返回 nil。
func normalize(vector []float32) []float32 {
	var sum float64
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	if sum == 0 {
		return nil
	}
	length := float32(math.Sqrt(sum))
	normalized := make([]float32, len(vector))
	for i, value := range vector {
		normalized[i] = value / length
	}
	return normalized
}
