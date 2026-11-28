package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 彻底删账号的清单不能漏：每个存储的表结构里按人存的（有 person 列的）包，要么登记了「删一个人的数据」
// （名字是 person_data.go 的常量，组装时以 accounts.NamedData{Name: <常量> 登记），要么写明为什么不删。
func TestPersonDataCoverage(t *testing.T) {
	registered := map[string]string{ // 包 → 登记用的常量
		"notes": "dataMemory", "reminders": "dataReminders", "calendar": "dataCalendar", "research": "dataResearch",
		"conversationstore": "dataConversations", "expenses": "dataExpenses",
	}
	kept := map[string]string{
		"accounts": "账号库自己：资料、家、终端、令牌随账号删（外键）",
		"spending": "花费账本：费用记录（谁确认过提高上限），不是个人内容",
	}
	files, err := filepath.Glob("../../internal/*/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("没找到表结构：%v", err)
	}
	personColumn := regexp.MustCompile(`(?m)^\s*(?:ALTER TABLE \w+ ADD COLUMN )?\w*person\s+TEXT`)
	found := map[string]bool{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if personColumn.MatchString(up) {
			found[filepath.Base(filepath.Dir(filepath.Dir(file)))] = true
		}
	}
	if !found["conversationstore"] { // 阳性对照：一定有 person 列的包认得出来
		t.Fatalf("没认出对话库的 person 列：%v", found)
	}
	var wiring strings.Builder
	for _, source := range []string{"assembly.go", "app_api.go"} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		wiring.Write(data)
	}
	for pkg := range found {
		if _, ok := kept[pkg]; ok {
			continue
		}
		name, ok := registered[pkg]
		if !ok {
			t.Errorf("%s 按人存数据，但彻底删账号时没删它（在 person_data.go 加一处、组装时登记，或者在这里写明为什么不删）", pkg)
			continue
		}
		if !strings.Contains(wiring.String(), "accounts.NamedData{Name: "+name) {
			t.Errorf("%s（%s）没在组装时登记", pkg, name)
		}
	}
}

// 删声纹：别的账号的本人经 haro-asr 删 account-<编号>；访客、管理员沿用的名字不调用。
func TestVoiceprintEraser(t *testing.T) {
	var deleted []string
	asr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, realtimeproto.VoiceprintPath))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer asr.Close()
	eraser := voiceprintEraser{asrURL: asr.URL, client: asr.Client()}
	owner, guest := accounts.PersonsOf("a1")
	for _, person := range []string{owner, guest, "主人"} {
		if err := eraser.ErasePerson(context.Background(), person); err != nil {
			t.Fatal(err)
		}
	}
	if len(deleted) != 1 || deleted[0] != "account-a1" {
		t.Errorf("删了：%v", deleted)
	}
}
