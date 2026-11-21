// Package pcmwav 是 16 位 PCM 与 WAV 之间的转换：解析整段 WAV、读边生成边返回的 WAV 的头、写 WAV 文件。
// 只依赖协议里的声音类型，识别服务、大脑、终端都能用。
package pcmwav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	goaudio "github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"github.com/bukahou/haro/internal/realtimeproto"
)

const (
	pcmFormat    = 1    // WAV fmt 块里的格式编号：1 = 不压缩的 PCM
	bitDepth     = 16   // 只处理 16 位样本
	maxFmtLength = 1024 // fmt 块最长 40 字节（可扩展格式）；再长就是坏数据，不按它分配内存
)

// Decode 解析一段完整的 16 位 PCM WAV（合成服务整段返回的、提示音文件、测试录音），返回裸 PCM 与格式。
//
// 块结构和格式由 go-audio/wav 解析；PCM 按 data 块的起点直接从原数据切出来：不逐个样本换算，
// 也不把 data 块后面的块当成声音（库自己读 PCM 时会一直读到末尾）。data 块长度为 0 或超出数据末尾时取到末尾：
// 边生成边写的 WAV 在头里先写占位值（0 或 0xFFFFFFFF；后者在库里按偶数对齐加一后溢出成 0）。
func Decode(data []byte) (realtimeproto.PCMAudio, error) {
	reader := bytes.NewReader(data)
	decoder := wav.NewDecoder(reader)
	err := decoder.FwdToPCM()
	if err == nil {
		err = decoder.Err() // 头解析失败时 FwdToPCM 返回 nil，错误只记在 Err 里
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("没有 fmt 块或 data 块")
		}
		return realtimeproto.PCMAudio{}, fmt.Errorf("不是能读的 WAV: %w", err)
	}
	format, err := checkFormat(int(decoder.WavAudioFormat), int(decoder.BitDepth), int(decoder.NumChans), int(decoder.SampleRate))
	if err != nil {
		return realtimeproto.PCMAudio{}, err
	}
	start := len(data) - reader.Len()
	end := len(data)
	if size := decoder.PCMSize; size > 0 && size < end-start {
		end = start + size
	}
	end -= (end - start) % (2 * format.Channels) // 只取完整的帧：对齐用的补位字节不是声音
	return realtimeproto.PCMAudio{Format: format, Data: data[start:end]}, nil
}

// ReadStreamHeader 从边生成边返回的 WAV（例如流式合成的 HTTP 响应）里读到 data 块的开头为止，返回声音格式；
// 之后 reader 里全是 PCM。data 块的长度不看：生成前不知道多长，写的是占位值。
//
// 这一处手写：go-audio/wav 的解码器要求能回退读取（io.ReadSeeker：fmt 块前有别的块时会倒回去重读），
// HTTP 响应只能往前读。
func ReadStreamHeader(reader io.Reader) (realtimeproto.AudioFormat, error) {
	riff := make([]byte, 12)
	if _, err := io.ReadFull(reader, riff); err != nil {
		return realtimeproto.AudioFormat{}, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return realtimeproto.AudioFormat{}, errors.New("开头不是 RIFF/WAVE")
	}
	var format realtimeproto.AudioFormat
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(reader, header); err != nil {
			return realtimeproto.AudioFormat{}, err
		}
		id, size := string(header[0:4]), int64(binary.LittleEndian.Uint32(header[4:8]))
		padded := size + size%2 // 块按偶数字节对齐
		switch id {
		case "data":
			if format.SampleRate == 0 {
				return realtimeproto.AudioFormat{}, errors.New("data 块之前没有 fmt 块")
			}
			return format, nil
		case "fmt ":
			if size < 16 || size > maxFmtLength {
				return realtimeproto.AudioFormat{}, fmt.Errorf("fmt 块长度不对（%d 字节）", size)
			}
			body := make([]byte, padded)
			if _, err := io.ReadFull(reader, body); err != nil {
				return realtimeproto.AudioFormat{}, err
			}
			var err error
			format, err = checkFormat(int(binary.LittleEndian.Uint16(body[0:2])), int(binary.LittleEndian.Uint16(body[14:16])),
				int(binary.LittleEndian.Uint16(body[2:4])), int(binary.LittleEndian.Uint32(body[4:8])))
			if err != nil {
				return realtimeproto.AudioFormat{}, err
			}
		default:
			if _, err := io.CopyN(io.Discard, reader, padded); err != nil {
				return realtimeproto.AudioFormat{}, err
			}
		}
	}
}

// WriteFile 把 16 位 PCM 写成 WAV 文件（生成提示音、测试时存下合成的声音）。
func WriteFile(path string, audio realtimeproto.PCMAudio) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	samples := make([]int, len(audio.Data)/2)
	for i := range samples {
		samples[i] = int(int16(binary.LittleEndian.Uint16(audio.Data[2*i:])))
	}
	encoder := wav.NewEncoder(file, audio.Format.SampleRate, bitDepth, audio.Format.Channels, pcmFormat)
	if err := encoder.Write(&goaudio.IntBuffer{
		Format:         &goaudio.Format{NumChannels: audio.Format.Channels, SampleRate: audio.Format.SampleRate},
		Data:           samples,
		SourceBitDepth: bitDepth,
	}); err != nil {
		return err
	}
	return encoder.Close()
}

// checkFormat 只认 16 位不压缩 PCM；声道数和采样率要大于 0。
func checkFormat(audioFormat, bits, channels, sampleRate int) (realtimeproto.AudioFormat, error) {
	if audioFormat != pcmFormat || bits != bitDepth {
		return realtimeproto.AudioFormat{}, fmt.Errorf("只支持 16 位 PCM 的 WAV（格式 %d，%d 位）", audioFormat, bits)
	}
	if channels < 1 || sampleRate < 1 {
		return realtimeproto.AudioFormat{}, fmt.Errorf("WAV 的声道数或采样率不对（%d 声道，%d Hz）", channels, sampleRate)
	}
	return realtimeproto.AudioFormat{SampleRate: sampleRate, Channels: channels}, nil
}
