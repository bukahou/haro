package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocationNote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"city":"Exampleville","region":"Example","country":"ZZ","loc":"12.3,45.6"}`))
	}))
	defer server.Close()
	home := NewIPHome(server.URL, "", server.Client())
	note := LocationNote{Home: home}
	if lines := note.Brief("主人"); lines != nil {
		t.Errorf("还没定位过：不该发网络请求，也不说位置：%v", lines)
	}
	if _, err := home.Home(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := note.Brief("主人")
	if len(lines) != 1 || !strings.Contains(lines[0], "Exampleville，Example，ZZ") || !strings.Contains(lines[0], "可能不准") ||
		strings.Contains(lines[0], "12.3") {
		t.Errorf("只说城市级（不说坐标），并注明按网络推定：%v", lines)
	}
	if lines := (LocationNote{Home: FixedHome{Place: Place{Latitude: 1, Longitude: 2}}}).Brief(""); lines != nil {
		t.Errorf("只有坐标、没有地名时不说：%v", lines)
	}
	lines = (LocationNote{Home: FixedHome{Place: Place{Name: "横浜市", Latitude: 1, Longitude: 2}}}).Brief("")
	if len(lines) != 1 || !strings.Contains(lines[0], "横浜市") || strings.Contains(lines[0], "可能不准") {
		t.Errorf("配置了名字的家：直接说，不注明推定：%v", lines)
	}
	if lines := (LocationNote{}).Brief(""); lines != nil {
		t.Errorf("没设家的位置：%v", lines)
	}
}
