package memory

import "testing"

// 按余弦相似度取前几名（和长短无关）；只在这一类、这几个人里找；维度不一样的跳过；去掉的不再出现。
func TestIndexSearch(t *testing.T) {
	index := NewIndex()
	index.Put(KindNote, "主人", 1, []float32{10, 0})   // 和问句同方向（长度不同）
	index.Put(KindNote, "", 2, []float32{1, 1})      // 全家共用，45°
	index.Put(KindNote, "老大", 3, []float32{1, 0})    // 别人的，同方向
	index.Put(KindTurn, "主人", 4, []float32{1, 0})    // 另一类
	index.Put(KindNote, "主人", 5, []float32{1, 0, 0}) // 维度不一样（换模型之前算的）
	index.Put(KindNote, "主人", 6, []float32{0, 0})    // 零向量不放
	if index.Len() != 5 {
		t.Fatalf("零向量不放：%d", index.Len())
	}
	hits := index.Search([]float32{2, 0}, KindNote, []string{"主人", ""}, 5)
	if len(hits) != 2 || hits[0].ID != 1 || hits[1].ID != 2 || hits[0].Score < 0.999 || hits[1].Score > 0.71 {
		t.Fatalf("主人和全家共用的、同类、同维度，按相似度排：%+v", hits)
	}
	if hits := index.Search([]float32{2, 0}, KindNote, []string{"主人", ""}, 1); len(hits) != 1 || hits[0].ID != 1 {
		t.Errorf("只取前 1 名：%+v", hits)
	}
	index.Remove(KindNote, "主人", 1)
	if hits := index.Search([]float32{2, 0}, KindNote, []string{"主人", ""}, 5); len(hits) != 1 || hits[0].ID != 2 {
		t.Errorf("去掉的不再出现：%+v", hits)
	}
	if hits := index.Search([]float32{0, 0}, KindNote, []string{"主人"}, 5); hits != nil {
		t.Errorf("零向量的问句没法比：%+v", hits)
	}
}
