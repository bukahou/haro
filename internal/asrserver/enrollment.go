package asrserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/bukahou/haro/internal/voiceprint"
)

// 远程登记声纹（v4 C-3）：终端录几句话发过来，这里算向量、存进声纹目录、立刻生效（不用重启）。
// 声纹存在服务端那边的数据目录里，终端不能直接写。
//
//	POST /v1/voiceprints/{name}   {"append": false, "clips": ["<base64 pcm16 16kHz 单声道>", …]}
//
// ⚠️ 声纹属于生物特征数据：只存在这台机器的声纹目录（0600），不进任何备份（v4 D8）。

// EnrollmentRequest 是登记请求。
type EnrollmentRequest struct {
	Append bool     `json:"append"` // 补录：加进这个人原来的声纹
	Clips  []string `json:"clips"`  // 每句一段 base64 的 pcm16（16 kHz 单声道）
}

// EnrollmentResult 是登记结果（给人看的诊断数字）。
type EnrollmentResult struct {
	Name           string       `json:"name"`
	Sentences      int          `json:"sentences"`                  // 存下后一共几句
	Pairwise       []PairResult `json:"pairwise"`                   // 这次几句之间的相似度
	BestVsExisting *float64     `json:"best_vs_existing,omitempty"` // 补录时：新录的和原来的最高相似度
	Speakers       []string     `json:"speakers"`                   // 登记后一共认得哪些人
}

// PairResult 是两句之间的相似度（句子从 1 数起）。
type PairResult struct {
	A, B  int
	Score float64
}

// minEnrollmentClips：至少几句（太少声纹不稳）。
const minEnrollmentClips = 2

func handleEnrollment(w http.ResponseWriter, r *http.Request, speakers *SpeakerCheck) {
	if speakers == nil || speakers.Embedder == nil || speakers.Dir == "" {
		writeError(w, http.StatusServiceUnavailable, "没有开声纹（haro-asr 没配说话人识别模型或声纹目录）")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	var request EnrollmentRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "无法解析请求: "+err.Error())
		return
	}
	if len(request.Clips) < minEnrollmentClips {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("至少要 %d 句", minEnrollmentClips))
		return
	}
	clips := make([][]float32, 0, len(request.Clips))
	for i, encoded := range request.Clips {
		pcm, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("第 %d 句不是合法的 base64", i+1))
			return
		}
		clips = append(clips, pcm16ToFloat(pcm))
	}
	result, err := speakers.enroll(r.PathValue("name"), clips, request.Append)
	var badRequest badEnrollment
	switch {
	case errors.As(err, &badRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

// badEnrollment 是请求本身的问题（名字不对、没登记过就补录、模型不一致）。
type badEnrollment struct{ error }

// enroll 算向量、存盘、重新加载所有声纹（立刻生效）。
func (c *SpeakerCheck) enroll(name string, clips [][]float32, appendMode bool) (EnrollmentResult, error) {
	result := EnrollmentResult{Name: name}
	embeddings := make([][]float32, 0, len(clips))
	for i, clip := range clips {
		embedding, err := c.Embedder.Embed(clip)
		if err != nil {
			return result, badEnrollment{fmt.Errorf("第 %d 句算不出声纹（太短或没有声音？）: %w", i+1, err)}
		}
		embeddings = append(embeddings, embedding)
	}
	for i := range embeddings {
		for j := i + 1; j < len(embeddings); j++ {
			result.Pairwise = append(result.Pairwise, PairResult{A: i + 1, B: j + 1,
				Score: voiceprint.Cosine(embeddings[i], embeddings[j])})
		}
	}
	if appendMode {
		existing, err := voiceprint.Load(c.Dir, name)
		if errors.Is(err, fs.ErrNotExist) {
			return result, badEnrollment{fmt.Errorf("「%s」还没登记过：先不带补录登记一次", name)}
		}
		if err != nil {
			return result, err
		}
		if existing.Model != c.Model {
			return result, badEnrollment{fmt.Errorf("原来的声纹是用 %s 算的，和现在的模型 %s 不一致：请重新登记", existing.Model, c.Model)}
		}
		var best float64
		for _, added := range embeddings {
			_, score := voiceprint.Match([]voiceprint.Voiceprint{existing}, added, 0)
			best = max(best, score)
		}
		result.BestVsExisting = &best
		embeddings = append(existing.Embeddings, embeddings...)
	}
	if _, err := voiceprint.Save(c.Dir, voiceprint.Voiceprint{Name: name, Model: c.Model, Embeddings: embeddings,
		EnrolledAt: time.Now()}); err != nil {
		return result, badEnrollment{err}
	}
	result.Sentences = len(embeddings)
	all, err := voiceprint.LoadAll(c.Dir, c.Model)
	if err != nil {
		return result, fmt.Errorf("已保存，但重新加载声纹失败（重启 haro-asr 后生效）: %w", err)
	}
	c.mu.Lock()
	c.Voiceprints = all
	c.mu.Unlock()
	for _, v := range all {
		result.Speakers = append(result.Speakers, v.Name)
	}
	return result, nil
}
