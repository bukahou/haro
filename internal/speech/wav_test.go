package speech

import (
	"bytes"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
)

func TestWAVRoundTrip(t *testing.T) {
	original := realtimeproto.PCMAudio{
		Format: realtimeproto.AudioFormat{SampleRate: 22050, Channels: 1},
		Data:   []byte{1, 2, 3, 4, 5, 6},
	}
	decoded, err := DecodeWAV(EncodeWAV(original))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Format != original.Format || !bytes.Equal(decoded.Data, original.Data) {
		t.Errorf("got %+v, want %+v", decoded, original)
	}
}

func TestDecodeWAVRejectsGarbage(t *testing.T) {
	if _, err := DecodeWAV([]byte("not a wav file at all")); err == nil {
		t.Error("期望返回错误")
	}
}
