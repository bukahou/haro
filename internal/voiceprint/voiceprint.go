// Package voiceprint 管登记过的声纹：存取（data/voiceprints/<名字>.json）和比对（v3 H-3）。纯 Go。
//
// 声纹向量由说话人识别模型算出（haro-asr 里用 sherpa-onnx 的 CAM++）；本包只管
// 「存在哪、是谁的、像不像」。比对用余弦相似度，分数在 Go 里算出来写进日志，方便真机调门槛。
//
// ⚠️ 声纹属于生物特征数据：只存本地（文件权限 0600），绝不上传云端（H-3b）。
package voiceprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// EnrollmentSentences 是登记时要读的话：第一句带唤醒词（唤醒时比对的就是喊唤醒词的声音），其余是日常说话。
// 终端的 haro -enroll（远程登记，v4 C-3）用。
var EnrollmentSentences = []string{
	"哈罗哈罗，今天天气怎么样？帮我看看外面会不会下雨。",
	"帮我记一下，周三下午要去取干洗的衣服，别让我忘了。",
	"我今天有点累，想早点休息，明天早上再说吧。",
}

// Voiceprint 是一个人登记的声纹：几句话各算一个向量。
type Voiceprint struct {
	Name       string      `json:"name"`
	Model      string      `json:"model"` // 算向量用的模型文件名（换模型后旧声纹不能比，要重新登记）
	Embeddings [][]float32 `json:"embeddings"`
	EnrolledAt time.Time   `json:"enrolled_at"`
}

// validName：名字直接做文件名，只允许字母、数字、汉字、下划线、连字符。
var validName = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,32}$`)

// Save 存到 dir/<名字>.json（权限 0600，先写临时文件再改名）。
func Save(dir string, voiceprint Voiceprint) (string, error) {
	if !validName.MatchString(voiceprint.Name) {
		return "", fmt.Errorf("名字 %q 不合法：只能用字母、数字、汉字、下划线、连字符，最长 32 个字", voiceprint.Name)
	}
	if len(voiceprint.Embeddings) == 0 {
		return "", errors.New("没有声纹向量")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	data, err := json.Marshal(voiceprint)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, voiceprint.Name+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// Load 读取一个人的声纹；没登记过时返回的错误满足 errors.Is(err, fs.ErrNotExist)。
func Load(dir, name string) (Voiceprint, error) {
	if !validName.MatchString(name) {
		return Voiceprint{}, fmt.Errorf("名字 %q 不合法", name)
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return Voiceprint{}, err
	}
	var voiceprint Voiceprint
	if err := json.Unmarshal(data, &voiceprint); err != nil {
		return Voiceprint{}, fmt.Errorf("解析声纹 %s: %w", name, err)
	}
	return voiceprint, nil
}

// LoadAll 读取 dir 下所有声纹（目录不存在 = 还没人登记，返回空）。只留用 model 算的（换了模型的旧声纹跳过）。
func LoadAll(dir, model string) ([]Voiceprint, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var voiceprints []Voiceprint
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var voiceprint Voiceprint
		if err := json.Unmarshal(data, &voiceprint); err != nil {
			return nil, fmt.Errorf("解析声纹 %s: %w", path, err)
		}
		if voiceprint.Model != model || len(voiceprint.Embeddings) == 0 {
			continue
		}
		voiceprints = append(voiceprints, voiceprint)
	}
	return voiceprints, nil
}

// Match 找和 embedding 最像的人：每个人取他几句登记里最像的一句的分数。
// 最高分不低于 threshold 才算认出来（返回名字），否则名字为空；分数总是返回（日志用）。
func Match(voiceprints []Voiceprint, embedding []float32, threshold float64) (string, float64) {
	bestName, bestScore := "", -1.0
	for _, voiceprint := range voiceprints {
		for _, enrolled := range voiceprint.Embeddings {
			if score := Cosine(enrolled, embedding); score > bestScore {
				bestName, bestScore = voiceprint.Name, score
			}
		}
	}
	if bestScore < threshold {
		return "", bestScore
	}
	return bestName, bestScore
}

// Cosine 返回两个向量的余弦相似度（长度不同或有零向量时为 0）。
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
