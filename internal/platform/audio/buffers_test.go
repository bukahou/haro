package audio

import (
	"bytes"
	"testing"
)

func TestChunkAssemblerMakesFixedChunks(t *testing.T) {
	var assembler chunkAssembler
	input := make([]byte, micChunkBytes*2+100)
	for i := range input {
		input[i] = byte(i)
	}
	chunks := assembler.push(input[:300])
	chunks = append(chunks, assembler.push(input[300:])...)
	if len(chunks) != 2 {
		t.Fatalf("应攒出 2 块，得到 %d", len(chunks))
	}
	if !bytes.Equal(append(chunks[0], chunks[1]...), input[:micChunkBytes*2]) {
		t.Error("块的内容和顺序应与输入一致")
	}
	if len(assembler.pending) != 100 {
		t.Errorf("剩下的 100 字节应留着等下一次：%d", len(assembler.pending))
	}
}

func TestPCMQueuePullsAndPadsSilence(t *testing.T) {
	var queue pcmQueue
	queue.push([]byte{1, 2, 3, 4})
	output := []byte{9, 9, 9, 9, 9, 9}
	queue.pull(output)
	if !bytes.Equal(output, []byte{1, 2, 3, 4, 0, 0}) {
		t.Errorf("不够的部分应填静音：%v", output)
	}
	if empty, _ := queue.status(); !empty || queue.consumedBytes() != 4 {
		t.Errorf("应已放空、记 4 字节：consumed=%d", queue.consumedBytes())
	}
	queue.deviceStopped()
	if queue.push([]byte{1, 2}) {
		t.Error("设备停了之后写入应失败")
	}
}
