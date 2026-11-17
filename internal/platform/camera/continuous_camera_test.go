package camera

import (
	"bufio"
	"bytes"
	"testing"
	"testing/iotest"
)

func TestSplitJPEGFrames(t *testing.T) {
	frameA := []byte{0xFF, 0xD8, 1, 2, 0xFF, 0x00, 3, 0xFF, 0xD9}
	frameB := []byte{0xFF, 0xD8, 9, 9, 0xFF, 0xD9}
	stream := bytes.Join([][]byte{{7, 7}, frameA, frameB, {0xFF, 0xD8, 5}}, nil) // 前有垃圾，后有半帧

	// 一次只给 1 个字节，模拟管道里数据零碎到达
	scanner := bufio.NewScanner(iotest.OneByteReader(bytes.NewReader(stream)))
	scanner.Split(splitJPEGFrames)
	var frames [][]byte
	for scanner.Scan() {
		frames = append(frames, bytes.Clone(scanner.Bytes()))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || !bytes.Equal(frames[0], frameA) || !bytes.Equal(frames[1], frameB) {
		t.Errorf("got %v", frames)
	}
}
