package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
)

// envExample 是配置样例里写的配置项：set 是没注释掉的（KEY="value"），defaults 是注释掉的（#KEY="value"，写的是默认值）。
type envExample struct {
	set, defaults map[string]string
}

func readEnvExample(t *testing.T, name string) envExample {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	example := envExample{set: map[string]string{}, defaults: map[string]string{}}
	for line := range strings.Lines(string(content)) {
		line = strings.TrimSpace(line)
		target := example.set
		if commented, ok := strings.CutPrefix(line, "#"); ok {
			line, target = commented, example.defaults
		}
		key, value, found := strings.Cut(line, "=")
		if found && strings.HasPrefix(key, "HARO_") && !strings.ContainsAny(key, " \t") {
			target[key] = strings.Trim(value, `"`)
		}
	}
	if len(example.set)+len(example.defaults) == 0 {
		t.Fatalf("%s 里没读到配置项", name)
	}
	return example
}

func (e envExample) keys() map[string]bool {
	keys := map[string]bool{}
	for _, values := range []map[string]string{e.set, e.defaults} {
		for key := range values {
			keys[key] = true
		}
	}
	return keys
}

// checkDefaults 检查样例里注释掉的值确实是默认值：加上它解析出来的配置和不加时一样。
func checkDefaults[T any](t *testing.T, name string, example envExample) {
	t.Helper()
	parse := func(values map[string]string) T {
		var settings T
		if err := env.ParseWithOptions(&settings, env.Options{Environment: values}); err != nil {
			t.Fatalf("%s 的值解析不了：%v", name, err)
		}
		return settings
	}
	base := parse(example.set)
	for _, key := range slices.Sorted(maps.Keys(example.defaults)) {
		values := maps.Clone(example.set)
		values[key] = example.defaults[key]
		if !reflect.DeepEqual(parse(values), base) {
			t.Errorf("%s 里注释掉的 %s=%q 不是默认值", name, key, example.defaults[key])
		}
	}
}

// 配置样例里写的都是现在真有的配置项、注释掉的写的确实是默认值（改名、删掉配置项、改默认值时样例跟着改）；
// 终端的样例列全了终端的每一项。
func TestEnvExamplesMatchSettings(t *testing.T) {
	const terminalExample, serverExample = "deploy/systemd/terminal.env.example", "deploy/server/server.env.example"
	terminal := envNames[TerminalSettings]()
	maps.Copy(terminal, envNames[Common]())
	example := readEnvExample(t, terminalExample)
	listed := example.keys()
	for key := range listed {
		if !terminal[key] {
			t.Errorf("终端的样例里有 %s，终端的配置里没有这一项", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(terminal)) {
		if !listed[key] {
			t.Errorf("终端的样例里缺 %s", key)
		}
	}
	checkDefaults[TerminalSettings](t, terminalExample, example)

	server := envNames[ServerSettings]()
	maps.Copy(server, envNames[Common]())
	maps.Copy(server, envNames[ASRSettings]()) // 服务端和 haro-asr 可以共用一份
	example = readEnvExample(t, serverExample)
	for key := range example.keys() {
		if !server[key] {
			t.Errorf("服务端的样例里有 %s，服务端（和 haro-asr）的配置里没有这一项", key)
		}
	}
	checkDefaults[ServerSettings](t, serverExample, example)
}
