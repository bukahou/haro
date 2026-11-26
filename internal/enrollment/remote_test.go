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

// fakeServer 像服务端那样：GET 登记句子时给 sentences，POST 登记时交给 enroll。
func fakeServer(t *testing.T, sentences []string, enroll http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == realtimeproto.EnrollmentScriptPath {
			_ = json.NewEncoder(w).Encode(realtimeproto.EnrollmentScript{Sentences: sentences})
			return
		}
		enroll(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRemoteEnrollmentUploadsClips(t *testing.T) {
	var got struct {
		Append bool     `json:"append"`
		Clips  []string `json:"clips"`
	}
	script := voiceprint.EnrollmentSentences("哈罗哈罗")
	brain := fakeServer(t, script, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/voiceprints/%E4%B8%BB%E4%BA%BA" {
			t.Errorf("路径：%s", r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"name":"主人","sentences":9,"pairwise":[{"a":1,"b":2,"score":0.84}],"best_vs_existing":0.73,"speakers":["主人"]}`))
	})

	sentences := len(script)
	var out bytes.Buffer
	recorder := &fakeRecorder{amplitudes: []int16{10}} // 第一句太小声：重读
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", Append: true,
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

// 不是补录时也写明 append=false：服务端要求写明补录还是覆盖，没写的不按覆盖处理。
func TestRemoteEnrollmentStatesAppendExplicitly(t *testing.T) {
	var body map[string]json.RawMessage
	script := voiceprint.EnrollmentSentences("哈罗哈罗")
	brain := fakeServer(t, script, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"name":"主人","sentences":3,"speakers":["主人"]}`))
	})
	sentences := len(script)
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", Seconds: time.Millisecond,
		Recorder: &fakeRecorder{}, In: strings.NewReader(strings.Repeat("\n", sentences)), Out: &bytes.Buffer{}, Client: brain.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if sent := string(body["append"]); sent != "false" {
		t.Errorf("不是补录时要写明 append=false，发出去的是 %q", sent)
	}
}

// 要读的句子照服务端给的提示（第一句以 haro-asr 关键词文件里的唤醒词开头：换了唤醒词，登记的就是喊新唤醒词的声音）；
// 服务端没给句子时不开始录。
func TestRemoteEnrollmentReadsSentencesFromServer(t *testing.T) {
	script := voiceprint.EnrollmentSentences("小助手小助手")
	brain := fakeServer(t, script, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"主人","sentences":3,"speakers":["主人"]}`))
	})
	var out bytes.Buffer
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", Seconds: time.Millisecond,
		Recorder: &fakeRecorder{}, In: strings.NewReader(strings.Repeat("\n", len(script))), Out: &out, Client: brain.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "「"+script[0]+"」") {
		t.Errorf("应照服务端给的第一句提示：%q\n%s", script[0], out.String())
	}

	noScript := fakeServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		t.Error("没拿到句子就不该上传")
	})
	recorder := &fakeRecorder{}
	err = Run(context.Background(), Options{ServerURL: noScript.URL, Name: "主人", Seconds: time.Millisecond, Recorder: recorder,
		In: strings.NewReader(strings.Repeat("\n", 3)), Out: &bytes.Buffer{}, Client: noScript.Client()})
	if err == nil || recorder.recordings != 0 {
		t.Errorf("服务端没给句子应直接报错、不开始录：%v，录了 %d 次", err, recorder.recordings)
	}
}

// 服务端给不了句子（例如 haro-asr 没有唤醒词，回 503）时把它说的原因告诉用户。
func TestRemoteEnrollmentReportsScriptError(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"haro-asr 没有唤醒词","type":"server_error"}}`))
	}))
	defer brain.Close()
	recorder := &fakeRecorder{}
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", Seconds: time.Millisecond, Recorder: recorder,
		In: strings.NewReader("\n"), Out: &bytes.Buffer{}, Client: brain.Client()})
	if err == nil || !strings.Contains(err.Error(), "没有唤醒词") || recorder.recordings != 0 {
		t.Errorf("应说清楚服务端的原因、不开始录：%v，录了 %d 次", err, recorder.recordings)
	}
}

// 音量门槛可以指定（MinLevel）：比它小声的那句要重读；不指定时用 DefaultMinLevel。
func TestRemoteEnrollmentMinLevelOption(t *testing.T) {
	script := voiceprint.EnrollmentSentences("哈罗哈罗")
	brain := fakeServer(t, script, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"主人","sentences":3,"speakers":["主人"]}`))
	})
	sentences := len(script)
	for _, c := range []struct {
		name       string
		minLevel   float64
		recordings int
	}{
		{"门槛 0.5：第一句（约 0.24）要重读", 0.5, sentences + 1},
		{"不指定：约 0.24 远高于默认门槛，不用重读", 0, sentences},
	} {
		recorder := &fakeRecorder{amplitudes: []int16{8000, 30000, 30000, 30000}} // 之后几句约 0.92
		err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", MinLevel: c.minLevel,
			Seconds: time.Millisecond, Recorder: recorder, In: strings.NewReader(strings.Repeat("\n", sentences+1)),
			Out: &bytes.Buffer{}, Client: brain.Client()})
		if err != nil || recorder.recordings != c.recordings {
			t.Errorf("%s：录了 %d 次，应为 %d 次（%v）", c.name, recorder.recordings, c.recordings, err)
		}
	}
}

func TestRemoteEnrollmentReportsBrainError(t *testing.T) {
	brain := fakeServer(t, voiceprint.EnrollmentSentences("哈罗哈罗"), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"「主人」还没登记过","type":"invalid_request_error"}}`))
	})
	err := Run(context.Background(), Options{ServerURL: brain.URL, Name: "主人", Append: true,
		Seconds: time.Millisecond, Recorder: &fakeRecorder{}, In: strings.NewReader(strings.Repeat("\n", 5)), Out: &bytes.Buffer{},
		Client: brain.Client()})
	if err == nil || !strings.Contains(err.Error(), "还没登记过") {
		t.Errorf("应把服务端的错误说清楚：%v", err)
	}
}
