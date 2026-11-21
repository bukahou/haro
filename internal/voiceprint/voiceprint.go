// Package voiceprint 管登记过的声纹：存取（data/voiceprints/<名字>.json）和比对。纯 Go。
//
// 声纹向量由说话人识别模型算出（haro-asr 里用 sherpa-onnx 的 CAM++）；本包只管
// 「存在哪、是谁的、像不像」。比对用余弦相似度，分数在 Go 里算出来写进日志，方便真机调门槛。
//
// ⚠️ 声纹属于生物特征数据：只存本地（文件权限 0600），绝不上传云端。
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

// EnrollmentSentences 是登记时要读的话（给人朗读的文本）：第一句以唤醒词开头（唤醒时比对的就是喊唤醒词的声音），
// 其余是日常说话。wakePhrase 是终端配置的唤醒词（HARO_WAKE_PHRASE）。终端的 haro -enroll 用。
func EnrollmentSentences(wakePhrase string) []string {
	return []string{
		wakePhrase + "，今天天气怎么样？帮我看看外面会不会下雨。",
		"帮我记一下，周三下午要去取干洗的衣服，别让我忘了。",
		"我今天有点累，想早点休息，明天早上再说吧。",
	}
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

// ErrInvalidName 是名字不能做文件名（调用方给错了，不是存储出了问题）。
var ErrInvalidName = errors.New("名字只能用字母、数字、汉字、下划线、连字符，最长 32 个字")

// CheckName 检查名字能不能用；不能用时返回的错误满足 errors.Is(err, ErrInvalidName)。
func CheckName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("名字 %q 不合法: %w", name, ErrInvalidName)
	}
	return nil
}

// Save 存到 dir/<名字>.json（权限 0600）：先写同目录下名字唯一的临时文件、刷到盘上，再改名替换。同时存同一个人
// 也不会写到同一个临时文件里；读的人只会看到完整的旧文件或新文件。
func Save(dir string, voiceprint Voiceprint) (string, error) {
	if err := CheckName(voiceprint.Name); err != nil {
		return "", err
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
	if err := replaceFile(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// replaceFile 用 data 替换 path：写进同目录下 os.CreateTemp 建的临时文件（0600，名字不以 .json 结尾，LoadAll 不会读到），
// 刷到盘上再改名。中途出错时删掉临时文件。
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// Load 读取一个人的声纹；没登记过时返回的错误满足 errors.Is(err, fs.ErrNotExist)。
func Load(dir, name string) (Voiceprint, error) {
	if err := CheckName(name); err != nil {
		return Voiceprint{}, err
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
