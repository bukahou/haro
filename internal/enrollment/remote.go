// Package enrollment 是终端一侧的远程登记声纹（`haro -enroll 名字`）：在终端这台机器上录几句话，
// 发给服务端（服务端转给 haro-asr 算向量、存进服务端的数据目录、立刻生效）。终端不需要说话人识别模型。
//
// ⚠️ 录音经 HTTP POST 发给服务端（和对话连的是同一个地址 HARO_SERVER_URL；没有加密和鉴权，只在可信网络里用）；
// 声纹只存在服务端那边。
package enrollment

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/endpoint"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/voiceprint"
)

// Options 是一次登记的参数。
type Options struct {
	ServerURL  string // 服务端的地址（同 HARO_SERVER_URL）
	Name       string
	WakePhrase string        // 终端配置的唤醒词（HARO_WAKE_PHRASE）：登记的第一句以它开头
	Append     bool          // 补录（换了麦克风、换了位置时）
	Seconds    time.Duration // 每句录多久
	MinLevel   float64       // 一句的音量（RMS，0–1）低于这个就让用户重读；0 = DefaultMinLevel
	Recorder   assistant.AudioRecorder
	In         io.Reader // 按回车开始录（一般是 os.Stdin）
	Out        io.Writer
	Client     *http.Client
}

// DefaultMinLevel 是默认的音量门槛（RMS）。定值，没有单独实测：约 -46 dBFS，只拦下几乎没录到声音的情况
// （麦克风静音、离得太远）。
const DefaultMinLevel = 0.005

// Run 录 voiceprint.EnrollmentSentences 里的每一句，发给服务端登记，打印结果。
func Run(ctx context.Context, opts Options) error {
	if opts.Name == "" {
		return fmt.Errorf("要给出登记的名字")
	}
	if opts.WakePhrase == "" {
		return fmt.Errorf("要给出唤醒词（登记的第一句以它开头）")
	}
	minLevel := opts.MinLevel
	if minLevel <= 0 {
		minLevel = DefaultMinLevel
	}
	sentences := voiceprint.EnrollmentSentences(opts.WakePhrase)
	if opts.Append {
		fmt.Fprintf(opts.Out, "补录「%s」：录 %d 句，加进原来的声纹\n", opts.Name, len(sentences))
	}
	input := bufio.NewReader(opts.In)
	var clips []string
	for i := 0; i < len(sentences); {
		fmt.Fprintf(opts.Out, "\n第 %d/%d 句。按回车后，用平时说话的音量读：\n    「%s」\n", i+1, len(sentences), sentences[i])
		if _, err := input.ReadString('\n'); err != nil {
			return err
		}
		pcm, err := recordFor(ctx, opts.Recorder, opts.Seconds, opts.Out)
		if err != nil {
			return err
		}
		if level := rms(pcm); level < minLevel {
			fmt.Fprintf(opts.Out, "⚠️ 声音太小（音量 %.4f），离麦克风近一点，再读一次。\n", level)
			continue
		}
		clips = append(clips, base64.StdEncoding.EncodeToString(pcm))
		i++
	}
	result, err := upload(ctx, opts, clips)
	if err != nil {
		return err
	}
	fmt.Fprintln(opts.Out, "\n几句之间的相似度（越接近 1 越像，正常应在 0.6 以上）：")
	for _, pair := range result.Pairwise {
		fmt.Fprintf(opts.Out, "    第 %d 句 ↔ 第 %d 句：%.2f\n", pair.A, pair.B, pair.Score)
	}
	if result.BestVsExisting != nil {
		fmt.Fprintf(opts.Out, "和原来登记的最高相似度：%.2f（换了麦克风或位置时低一些是正常的）\n", *result.BestVsExisting)
	}
	fmt.Fprintf(opts.Out, "\n✅ 已登记「%s」（共 %d 句），已经生效。现在认得：%s\n", result.Name, result.Sentences,
		strings.Join(result.Speakers, "、"))
	return nil
}

func recordFor(ctx context.Context, recorder assistant.AudioRecorder, seconds time.Duration, out io.Writer) ([]byte, error) {
	recording, err := recorder.StartRecording(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "🔴 录音中（%s）…\n", seconds)
	select {
	case <-time.After(seconds):
	case <-ctx.Done():
		_, _ = recording.Stop()
		return nil, ctx.Err()
	}
	audio, err := recording.Stop()
	return audio.Data, err
}

func upload(ctx context.Context, opts Options, clips []string) (realtimeproto.VoiceprintEnrollmentResult, error) {
	var none realtimeproto.VoiceprintEnrollmentResult
	body, err := json.Marshal(realtimeproto.VoiceprintEnrollment{Append: &opts.Append, Clips: clips}) // 补录还是覆盖都写明
	if err != nil {
		return none, err
	}
	address, err := endpoint.Join(opts.ServerURL, realtimeproto.VoiceprintPath, nil, opts.Name)
	if err != nil {
		return none, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return none, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", buildinfo.UserAgent())
	request.Header.Set(buildinfo.Header, buildinfo.Version)
	response, err := opts.Client.Do(request)
	if err != nil {
		return none, fmt.Errorf("连不上服务端 %s: %w", opts.ServerURL, err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		var failure realtimeproto.HTTPError
		if json.Unmarshal(data, &failure) == nil && failure.Error.Message != "" {
			return none, fmt.Errorf("登记失败：%s", failure.Error.Message)
		}
		return none, fmt.Errorf("登记失败：HTTP %d %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var parsed realtimeproto.VoiceprintEnrollmentResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		return none, fmt.Errorf("解析登记结果: %w", err)
	}
	return parsed, nil
}

// rms 是 pcm16 的音量（0–1）。
func rms(pcm []byte) float64 {
	samples := len(pcm) / 2
	if samples == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < samples; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(samples))
}
