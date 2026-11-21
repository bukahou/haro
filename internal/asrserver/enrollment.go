package asrserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/voiceprint"
)

// 远程登记声纹：终端录几句话发过来，这里算向量、存进声纹目录、立刻生效（不用重启）。
// 声纹存在服务端那边的数据目录里，终端不能直接写。
//
// 请求和响应的结构见 realtimeproto.VoiceprintEnrollment。
//
// ⚠️ 声纹属于生物特征数据：只存在这台机器的声纹目录（0600），不进任何备份。

// maxUploadBytes 是一次登记声纹请求体的上限：几句话的 base64 音频远到不了这么大。
const maxUploadBytes = 32 << 20

// minEnrollmentClips：至少几句（太少声纹不稳）。
const minEnrollmentClips = 2

func handleEnrollment(w http.ResponseWriter, r *http.Request, speakers *SpeakerCheck) {
	if speakers == nil || speakers.Embedder == nil || speakers.Dir == "" {
		writeError(w, http.StatusServiceUnavailable, "没有开声纹（haro-asr 没配说话人识别模型或声纹目录）")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	var request realtimeproto.VoiceprintEnrollment
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

// badEnrollment 是请求本身的问题（名字不对、句子算不出声纹、没登记过就补录、模型不一致）：回 400。
// 其他错误（存盘、读盘失败）是服务端的问题：回 500。
type badEnrollment struct{ error }

// enroll 算向量、存盘、重新加载所有声纹（立刻生效）。
func (c *SpeakerCheck) enroll(name string, clips [][]float32, appendMode bool) (realtimeproto.VoiceprintEnrollmentResult, error) {
	result := realtimeproto.VoiceprintEnrollmentResult{Name: name}
	if err := voiceprint.CheckName(name); err != nil {
		return result, badEnrollment{err}
	}
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
			result.Pairwise = append(result.Pairwise, realtimeproto.VoiceprintPair{A: i + 1, B: j + 1,
				Score: voiceprint.Cosine(embeddings[i], embeddings[j])})
		}
	}
	// 存盘一次一个：补录要读出原来的声纹、加上新的再存回去，两次登记同时做会互相覆盖；存完重新加载也按同样的先后
	c.enrollMu.Lock()
	defer c.enrollMu.Unlock()
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
		return result, fmt.Errorf("保存声纹: %w", err)
	}
	result.Sentences = len(embeddings)
	all, _, err := voiceprint.LoadAll(c.Dir, c.Model) // 用不上的那几份（换了模型的）启动时已经逐个记过
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
