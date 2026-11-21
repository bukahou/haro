package pcmwav

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// chunk 是测试里手拼 WAV 用的一个块。
type chunk struct {
	id   string
	size uint32 // 头里写的长度
	body []byte
}

// buildWAV 手拼一段 WAV：RIFF 头 + 各块（头里的长度照 chunk.size 写，可以故意写错）。
func buildWAV(chunks ...chunk) []byte {
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))
	out.WriteString("WAVE")
	for _, c := range chunks {
		out.WriteString(c.id)
		_ = binary.Write(&out, binary.LittleEndian, c.size)
		out.Write(c.body)
	}
	return out.Bytes()
}

// fmtChunk 是 fmt 块：格式编号、声道数、采样率、位深。
func fmtChunk(audioFormat, channels uint16, sampleRate uint32, bits uint16) chunk {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:], audioFormat)
	binary.LittleEndian.PutUint16(body[2:], channels)
	binary.LittleEndian.PutUint32(body[4:], sampleRate)
	binary.LittleEndian.PutUint32(body[8:], sampleRate*uint32(channels)*uint32(bits/8))
	binary.LittleEndian.PutUint16(body[12:], channels*bits/8)
	binary.LittleEndian.PutUint16(body[14:], bits)
	return chunk{id: "fmt ", size: 16, body: body}
}

var pcm = []byte{1, 2, 3, 4, 5, 6, 7, 8}

// 写成文件再读回来：格式、数据都不变（单声道、双声道）。
func TestWriteFileThenDecode(t *testing.T) {
	for _, format := range []realtimeproto.AudioFormat{{SampleRate: 22050, Channels: 1}, {SampleRate: 48000, Channels: 2}} {
		path := filepath.Join(t.TempDir(), "a.wav")
		original := realtimeproto.PCMAudio{Format: format, Data: pcm}
		if err := WriteFile(path, original); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(data)
		if err != nil || decoded.Format != format || !bytes.Equal(decoded.Data, pcm) {
			t.Errorf("%+v：读回 %+v %v", format, decoded, err)
		}
	}
}

// data 块前后有别的块：只取 data 块里的声音（后面的块不当成声音）；fmt 块前有别的块也能读。
func TestDecodeTakesOnlyTheDataChunk(t *testing.T) {
	list := chunk{id: "LIST", size: 4, body: []byte("INFO")}
	junk := chunk{id: "JUNK", size: 6, body: []byte("abcdef")}
	data := buildWAV(junk, fmtChunk(1, 1, 16000, 16), list, chunk{id: "data", size: uint32(len(pcm)), body: pcm}, list)
	decoded, err := Decode(data)
	if err != nil || decoded.Format != (realtimeproto.AudioFormat{SampleRate: 16000, Channels: 1}) || !bytes.Equal(decoded.Data, pcm) {
		t.Errorf("应只取 data 块：%+v %v", decoded, err)
	}
}

// 边生成边写的 WAV：data 块长度是占位值（0、0xFFFFFFFF、比实际长）时取到末尾；末尾不成帧的字节不要。
func TestDecodePlaceholderDataLength(t *testing.T) {
	for _, size := range []uint32{0, 0xFFFFFFFF, 1000} {
		data := buildWAV(fmtChunk(1, 2, 24000, 16), chunk{id: "data", size: size, body: append(append([]byte{}, pcm...), 9, 9)})
		decoded, err := Decode(data)
		if err != nil || !bytes.Equal(decoded.Data, pcm) {
			t.Errorf("长度 %#x：应取到末尾的完整帧：%v %v", size, decoded.Data, err)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"不是 WAV":    []byte("not a wav file at all"),
		"8 位":       buildWAV(fmtChunk(1, 1, 16000, 8), chunk{id: "data", size: 4, body: pcm[:4]}),
		"浮点":        buildWAV(fmtChunk(3, 1, 16000, 32), chunk{id: "data", size: 4, body: pcm[:4]}),
		"没有 data 块": buildWAV(fmtChunk(1, 1, 16000, 16)),
		"没有 fmt 块":  buildWAV(chunk{id: "data", size: 4, body: pcm[:4]}),
		"声道数是 0":    buildWAV(fmtChunk(1, 0, 16000, 16), chunk{id: "data", size: 4, body: pcm[:4]}),
	} {
		if audio, err := Decode(data); err == nil {
			t.Errorf("%s：应报错，得到 %+v", name, audio)
		}
	}
}

// 流式的头：data 长度是 0（生成前不知道多长），fmt 前有别的块也能跳过；读完后剩下的全是 PCM。
func TestReadStreamHeader(t *testing.T) {
	stream := buildWAV(chunk{id: "JUNK", size: 3, body: []byte("abc\x00")}, fmtChunk(1, 1, 32000, 16), chunk{id: "data", size: 0, body: pcm})
	reader := bufio.NewReader(bytes.NewReader(stream))
	format, err := ReadStreamHeader(reader)
	if err != nil || format != (realtimeproto.AudioFormat{SampleRate: 32000, Channels: 1}) {
		t.Fatalf("格式：%+v %v", format, err)
	}
	if rest, _ := io.ReadAll(reader); !bytes.Equal(rest, pcm) {
		t.Errorf("头后面应全是 PCM：%v", rest)
	}
}

func TestReadStreamHeaderRejects(t *testing.T) {
	huge := chunk{id: "fmt ", size: 0x7FFFFFFF}
	for name, stream := range map[string][]byte{
		"不是 WAV":        []byte("not a wav file at all"),
		"8 位":           buildWAV(fmtChunk(1, 1, 16000, 8), chunk{id: "data"}),
		"fmt 之前就是 data": buildWAV(chunk{id: "data"}, fmtChunk(1, 1, 16000, 16)),
		"fmt 块长得离谱":     buildWAV(huge),
		"没读到 data 就断了":  buildWAV(fmtChunk(1, 1, 16000, 16)),
	} {
		if format, err := ReadStreamHeader(strings.NewReader(string(stream))); err == nil {
			t.Errorf("%s：应报错，得到 %+v", name, format)
		}
	}
}

// fmt 块的长度写得离谱（坏数据）：直接报错，不按它分配内存。
func TestReadStreamHeaderDoesNotTrustHugeFmtLength(t *testing.T) {
	stream := buildWAV(chunk{id: "fmt ", size: 0x7FFFFFFF})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := ReadStreamHeader(bytes.NewReader(stream)); err == nil {
		t.Error("应报错")
	}
	runtime.ReadMemStats(&after)
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
		t.Errorf("不该按坏长度分配内存：分配了 %d 字节", grown)
	}
}
