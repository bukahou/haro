package brain

import "testing"

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
