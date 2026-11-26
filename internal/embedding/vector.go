package embedding

import (
	"encoding/binary"
	"fmt"
	"math"
)

// 向量存进库时的格式只定义在这里（记事库、对话库都用它）：每个分量一个 float32，小端，依次排开。

// EncodeVector 把向量编成存库用的字节。
func EncodeVector(vector []float32) []byte {
	data := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(value))
	}
	return data
}

// DecodeVector 把库里的字节解回向量；长度不是 4 的倍数（或为空）时报错。
func DecodeVector(data []byte) ([]float32, error) {
	if len(data) == 0 || len(data)%4 != 0 {
		return nil, fmt.Errorf("向量的字节数（%d）不对", len(data))
	}
	vector := make([]float32, len(data)/4)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
	}
	return vector, nil
}
