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
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// EnrollmentSentences 是登记时要读的话（给人朗读的文本）：第一句以唤醒词开头（唤醒时比对的就是喊唤醒词的声音），
// 其余是日常说话。wakePhrase 是 haro-asr 关键词文件里的唤醒词；haro-asr 经登记句子的接口交给终端的 haro -enroll。
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

// ErrOtherModel：这份声纹是用别的说话人识别模型算的，和现在的模型不能比（换模型后要重新登记）。
var ErrOtherModel = errors.New("声纹是用别的说话人识别模型算的，要重新登记")

// ErrNoEmbeddings：这份声纹里没有向量。
var ErrNoEmbeddings = errors.New("声纹里没有向量")

// Skipped 是读到了、但用不上的一份声纹：调用方逐个记下来（不然这个人一直被当成陌生人，看不出原因）。
type Skipped struct {
	File  string // 声纹文件
	Name  string
	Model string // 算这份声纹用的模型
	Err   error  // 为什么用不上：满足 errors.Is(err, ErrOtherModel) 或 errors.Is(err, ErrNoEmbeddings)
}

// LoadAll 读取 dir 下所有声纹，只留用 model 算的；用不上的（换了模型的旧声纹、没有向量的）列在 skipped 里。
// 目录不存在、读不了（没权限）时报错，不当成「还没人登记」：配了目录却读不到，多半是路径、挂载或权限不对。
// 目录要在第一次登记之前就有，由调用方建（还没人登记时它本来就是空的）。
func LoadAll(dir, model string) (voiceprints []Voiceprint, skipped []Skipped, err error) {
	entries, err := os.ReadDir(dir) // 和 filepath.Glob 不同：读不了目录时报错
	if err != nil {
		return nil, nil, fmt.Errorf("读取声纹目录: %w", err)
	}
	for _, entry := range entries { // 按文件名排好序了
		if filepath.Ext(entry.Name()) != ".json" {
			continue // 存盘用的临时文件（.tmp）等
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		var voiceprint Voiceprint
		if err := json.Unmarshal(data, &voiceprint); err != nil {
			return nil, nil, fmt.Errorf("解析声纹 %s: %w", path, err)
		}
		switch {
		case voiceprint.Model != model:
			skipped = append(skipped, Skipped{File: path, Name: voiceprint.Name, Model: voiceprint.Model,
				Err: fmt.Errorf("%w（它用的是 %s，现在是 %s）", ErrOtherModel, voiceprint.Model, model)})
		case len(voiceprint.Embeddings) == 0:
			skipped = append(skipped, Skipped{File: path, Name: voiceprint.Name, Model: voiceprint.Model, Err: ErrNoEmbeddings})
		default:
			voiceprints = append(voiceprints, voiceprint)
		}
	}
	return voiceprints, skipped, nil
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

// Remove 删掉 dir/<名字>.json（彻底删账号时）；本来就没有不算错。
func Remove(dir, name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, name+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
