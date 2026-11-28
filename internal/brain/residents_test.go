package brain

import (
	"context"
	"testing"
)

// 分账号的终端只认本人：别人的声音是访客、不算声纹确认、名字不告诉终端；不分账号时照旧。
func TestResidents(t *testing.T) {
	account := Residents{Owner: "account:a", Guest: "account:a:guest"}
	for _, check := range []struct {
		residents          Residents
		speaker, person    string
		verified           bool
		reported, briefing string
	}{
		{account, "account:a", "account:a", true, "account:a", "现在说话的是这台终端的主人（按声纹认出）。"},
		{account, "主人", "account:a:guest", false, "account:a:guest", ""},
		{Residents{}, "主人", "主人", true, "主人", "现在说话的是：主人（按声纹认出）。"},
	} {
		person := check.residents.personFor(check.speaker)
		if person != check.person || check.residents.verified(check.speaker) != check.verified ||
			check.residents.reported(check.speaker) != check.reported || check.residents.speakerLine(person) != check.briefing {
			t.Errorf("%+v 说话的是 %q：算成 %q、确认 %v、告诉终端 %q、说明 %q", check.residents, check.speaker, person,
				check.residents.verified(check.speaker), check.residents.reported(check.speaker), check.residents.speakerLine(person))
		}
	}
	if account.shared() != "account:a:guest" || (Residents{}).shared() != SharedPerson {
		t.Error("还没认出时用的人")
	}
	if account.reported("") != "" || account.verified("") {
		t.Error("没认出的照旧")
	}
}

// 别的账号的终端：声纹报的是本人的声纹名（account-<编号>）时算本人、算声纹确认；别的声音算访客。
func TestResidentsOwnerVoice(t *testing.T) {
	r := Residents{Owner: "account:a1", Guest: "account:a1:guest", OwnerVoice: "account-a1"}
	if got := r.personFor("account-a1"); got != "account:a1" || !r.verified("account-a1") {
		t.Errorf("本人：%q %v", got, r.verified("account-a1"))
	}
	if got := r.personFor("主人"); got != "account:a1:guest" || r.verified("主人") {
		t.Errorf("别人：%q %v", got, r.verified("主人"))
	}
}

// adminToolbox 有一个普通工具、一个只给管理员的工具。
type adminToolbox struct{}

func (adminToolbox) Specs() []ToolSpec {
	return []ToolSpec{{Name: "get_weather"}, {Name: "get_cluster_overview", AdminOnly: true}}
}
func (adminToolbox) Call(context.Context, ToolCall) (string, error) { return "", nil }

// 别的账号的会话（终端、App）拿不到只给管理员的工具；管理员的会话照常。
func TestAdminOnlyToolsHiddenFromOtherAccounts(t *testing.T) {
	names := func(r Residents) []string {
		s := &Session{parts: Components{Tools: adminToolbox{}, Residents: r}}
		var got []string
		for _, spec := range s.toolboxSpecs() {
			got = append(got, spec.Name)
		}
		return got
	}
	if got := names(Residents{}); len(got) != 2 {
		t.Errorf("管理员：%v", got)
	}
	if got := names(Residents{Owner: "account:a1", Guest: "account:a1:guest"}); len(got) != 1 || got[0] != "get_weather" {
		t.Errorf("别的账号：%v", got)
	}
}
