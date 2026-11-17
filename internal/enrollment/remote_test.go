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

	"haro/internal/assistant"
	"haro/internal/realtimeproto"
	"haro/internal/voiceprint"
)

// fakeRecorder 每次录出一段固定幅度的声音（第一次可以设成太小声）。
type fakeRecorder struct{ amplitudes []int16 }

func (f *fakeRecorder) StartRecording(context.Context) (assistant.Recording, error) {
	amplitude := int16(8000)
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
		_, _ = w.Write([]byte(`{"name":"主人","sentences":9,"pairwise":[{"A":1,"B":2,"Score":0.84}],"best_vs_existing":0.73,"speakers":["主人"]}`))
	}))
	defer brain.Close()

	sentences := len(voiceprint.EnrollmentSentences)
	var out bytes.Buffer
	err := Run(context.Background(), Options{BrainURL: brain.URL, Name: "主人", Append: true, Seconds: time.Millisecond,
		Recorder: &fakeRecorder{amplitudes: []int16{10}}, // 第一句太小声：重读
		In:       strings.NewReader(strings.Repeat("\n", sentences+1)), Out: &out, Client: brain.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Append || len(got.Clips) != sentences {
		t.Errorf("应补录、发 %d 句：%v %d", sentences, got.Append, len(got.Clips))
	}
	for _, want := range []string{"声音太小", "0.84", "0.73", "已登记「主人」（共 9 句）"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出里应有 %q：\n%s", want, out.String())
		}
	}
}

func TestRemoteEnrollmentReportsBrainError(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"「主人」还没登记过","type":"invalid_request_error"}}`))
	}))
	defer brain.Close()
	err := Run(context.Background(), Options{BrainURL: brain.URL, Name: "主人", Append: true, Seconds: time.Millisecond,
		Recorder: &fakeRecorder{}, In: strings.NewReader(strings.Repeat("\n", 5)), Out: &bytes.Buffer{}, Client: brain.Client()})
	if err == nil || !strings.Contains(err.Error(), "还没登记过") {
		t.Errorf("应把大脑的错误说清楚：%v", err)
	}
}
