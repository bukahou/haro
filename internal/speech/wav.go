package speech

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"haro/internal/realtimeproto"
)

// EncodeWAV 给 s16le PCM 加上 44 字节的 WAV 头（上传给只认文件格式的服务用）。
func EncodeWAV(audio realtimeproto.PCMAudio) []byte {
	var buf bytes.Buffer
	dataSize := uint32(len(audio.Data))
	blockAlign := uint16(audio.Format.Channels * 2)
	write := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.WriteString("RIFF")
	write(36 + dataSize)
	buf.WriteString("WAVEfmt ")
	write(uint32(16))                      // fmt 块长度
	write(uint16(1))                       // PCM
	write(uint16(audio.Format.Channels))   // 声道数
	write(uint32(audio.Format.SampleRate)) // 采样率
	write(uint32(audio.Format.BytesPerSecond()))
	write(blockAlign)
	write(uint16(16)) // 位深
	buf.WriteString("data")
	write(dataSize)
	buf.Write(audio.Data)
	return buf.Bytes()
}

// DecodeWAV 解析 16 位 PCM 的 WAV，返回裸 PCM 与格式。
func DecodeWAV(wav []byte) (realtimeproto.PCMAudio, error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return realtimeproto.PCMAudio{}, errors.New("不是 WAV 数据")
	}
	var format realtimeproto.AudioFormat
	gotFormat := false
	for offset := 12; offset+8 <= len(wav); {
		chunkID := string(wav[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(wav[offset+4 : offset+8]))
		body := offset + 8
		end := min(body+chunkSize, len(wav)) // 流式写出的 WAV 可能把长度写成占位值
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 || body+16 > len(wav) {
				return realtimeproto.PCMAudio{}, errors.New("WAV fmt 块太短")
			}
			audioFormat := binary.LittleEndian.Uint16(wav[body:])
			bitsPerSample := binary.LittleEndian.Uint16(wav[body+14:])
			if audioFormat != 1 || bitsPerSample != 16 {
				return realtimeproto.PCMAudio{}, fmt.Errorf("只支持 16 位 PCM（格式 %d，位深 %d）", audioFormat, bitsPerSample)
			}
			format.Channels = int(binary.LittleEndian.Uint16(wav[body+2:]))
			format.SampleRate = int(binary.LittleEndian.Uint32(wav[body+4:]))
			gotFormat = true
		case "data":
			if !gotFormat {
				return realtimeproto.PCMAudio{}, errors.New("WAV 缺少 fmt 块")
			}
			return realtimeproto.PCMAudio{Format: format, Data: wav[body:end]}, nil
		}
		offset = body + chunkSize + chunkSize%2 // 块按偶数字节对齐
	}
	return realtimeproto.PCMAudio{}, errors.New("WAV 缺少 data 块")
}
