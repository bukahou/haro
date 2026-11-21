package sherpa

import "os"

// tokensFile 是 sherpa-onnx 模型目录里词表的文件名（识别、唤醒词的模型包里都叫这个）。
const tokensFile = "tokens.txt"

// requireFiles 确认这些文件都在：sherpa-onnx 创建失败时只返回空指针，说不出是缺了哪个文件。
func requireFiles(paths ...string) error {
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			return err
		}
	}
	return nil
}
