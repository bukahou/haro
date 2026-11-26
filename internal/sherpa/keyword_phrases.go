package sherpa

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
)

// KeywordPhrases 读关键词文件里的唤醒词，按文件里的顺序、去掉重复：每行的显示名（以 @ 开头的那一段）；
// 没写显示名的行用它的拼音单元（去掉 :加分、#门槛）。空行和 # 开头的行跳过。
//
// 唤醒词这个事实只写在关键词文件一处：haro-asr 把这里读出的报给服务端（session.updated），服务端转给终端
// （日志提示），声纹登记的第一句也用它。格式是 sherpa-onnx 定的（每行「拼音单元 … [:加分] [#门槛] @显示名」，
// 各段用空白分开），它的 Go 接口没有列出关键词的函数，所以在这里读。
func KeywordPhrases(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var phrases []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var display string
		var units []string
		for _, field := range strings.Fields(line) {
			switch {
			case strings.HasPrefix(field, "@"):
				display = strings.TrimPrefix(field, "@")
			case strings.HasPrefix(field, ":"), strings.HasPrefix(field, "#"):
			default:
				units = append(units, field)
			}
		}
		if display == "" {
			display = strings.Join(units, " ")
		}
		if display != "" && !slices.Contains(phrases, display) {
			phrases = append(phrases, display)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(phrases) == 0 {
		return nil, fmt.Errorf("关键词文件 %s 里没有唤醒词", path)
	}
	return phrases, nil
}
