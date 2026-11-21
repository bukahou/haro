package enrollment

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/assistant"
	"github.com/bukahou/haro/internal/realtimeproto"
	"github.com/bukahou/haro/internal/voiceprint"
)

// fakeRecorder 每次录出一段固定幅度的声音（前几次的幅度可以指定，例如太小声），并数录了几次。
type fakeRecorder struct {
	amplitudes []int16
	recordings int
}

func (f *fakeRecorder) StartRecording(context.Context) (assistant.Recording, error) {
	f.recordings++
	amplitude := int16(8000) // 音量（RMS）约 0.24
	if len(f.amplitudes) > 0 {
		amplitude, f.amplitudes = f.amplitudes[0], f.amplitudes[1:]
	}
	pcm := make([]byte, 3200)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(amplitude))
	}
	return fakeRecording{pcm: pcm}, nil
}

type fakeRecording struct{ pcm []byte }

func (r fakeRecording) Stop() (realtimeproto.PCMAudio, error) {
	return realtimeproto.PCMAudio{Data: r.pcm}, nil
}

func TestRemoteEnrollmentUploadsClips(t *testing.T) {
	var got struct {
		Append bool     `json:"append"`
		Clips  []string `json:"clips"`
	}
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/voiceprints/%E4%B8%BB%E4%BA%BA" {
			t.Errorf("路径：%s", r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"name":"主人","sentences":9,"pairwise":[{"a":1,"b":2,"score":0.84}],"best_vs_existing":0.73,"speakers":["主人"]}`))
	}))
	defer brain.Close()

	sentences := len(voiceprint.EnrollmentSentences("哈罗哈罗"))
	var out bytes.Buffer
	recorder := &fakeRecorder{amplitudes: []int16{10}} // 第一句太小声：重读
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", WakePhrase: "哈罗哈罗", Append: true,
		Seconds: time.Millisecond, Recorder: recorder, In: strings.NewReader(strings.Repeat("\n", sentences+1)), Out: &out,
		Client: brain.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Append || len(got.Clips) != sentences || recorder.recordings != sentences+1 {
		t.Errorf("应补录、发 %d 句（太小声的那句重录一次）：%v %d 句，录了 %d 次", sentences, got.Append, len(got.Clips), recorder.recordings)
	}
	for _, want := range []string{"声音太小", "0.84", "0.73", "已登记「主人」（共 9 句）"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出里应有 %q：\n%s", want, out.String())
		}
	}
}

// 第一句以终端配置的唤醒词开头（换了唤醒词，登记的就是喊新唤醒词的声音）；没给唤醒词不开始录。
func TestRemoteEnrollmentFirstSentenceUsesWakePhrase(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"主人","sentences":3,"speakers":["主人"]}`))
	}))
	defer brain.Close()
	var out bytes.Buffer
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", WakePhrase: "小助手小助手", Seconds: time.Millisecond,
		Recorder: &fakeRecorder{}, In: strings.NewReader(strings.Repeat("\n", 3)), Out: &out, Client: brain.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if first := voiceprint.EnrollmentSentences("小助手小助手")[0]; !strings.HasPrefix(first, "小助手小助手") ||
		!strings.Contains(out.String(), "「"+first+"」") {
		t.Errorf("第一句应以配置的唤醒词开头并照它提示：%q\n%s", first, out.String())
	}

	recorder := &fakeRecorder{}
	err = Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", Seconds: time.Millisecond, Recorder: recorder,
		In: strings.NewReader(strings.Repeat("\n", 3)), Out: &bytes.Buffer{}, Client: brain.Client()})
	if err == nil || recorder.recordings != 0 {
		t.Errorf("没给唤醒词应直接报错、不开始录：%v，录了 %d 次", err, recorder.recordings)
	}
}

// 音量门槛可以指定（MinLevel）：比它小声的那句要重读；不指定时用 DefaultMinLevel。
func TestRemoteEnrollmentMinLevelOption(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"主人","sentences":3,"speakers":["主人"]}`))
	}))
	defer brain.Close()
	sentences := len(voiceprint.EnrollmentSentences("哈罗哈罗"))
	for _, c := range []struct {
		name       string
		minLevel   float64
		recordings int
	}{
		{"门槛 0.5：第一句（约 0.24）要重读", 0.5, sentences + 1},
		{"不指定：约 0.24 远高于默认门槛，不用重读", 0, sentences},
	} {
		recorder := &fakeRecorder{amplitudes: []int16{8000, 30000, 30000, 30000}} // 之后几句约 0.92
		err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", WakePhrase: "哈罗哈罗", MinLevel: c.minLevel,
			Seconds: time.Millisecond, Recorder: recorder, In: strings.NewReader(strings.Repeat("\n", sentences+1)),
			Out: &bytes.Buffer{}, Client: brain.Client()})
		if err != nil || recorder.recordings != c.recordings {
			t.Errorf("%s：录了 %d 次，应为 %d 次（%v）", c.name, recorder.recordings, c.recordings, err)
		}
	}
}

func TestRemoteEnrollmentReportsBrainError(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"「主人」还没登记过","type":"invalid_request_error"}}`))
	}))
	defer brain.Close()
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", WakePhrase: "哈罗哈罗", Append: true,
		Seconds: time.Millisecond, Recorder: &fakeRecorder{}, In: strings.NewReader(strings.Repeat("\n", 5)), Out: &bytes.Buffer{},
		Client: brain.Client()})
	if err == nil || !strings.Contains(err.Error(), "还没登记过") {
		t.Errorf("应把服务端的错误说清楚：%v", err)
	}
}
