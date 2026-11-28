package accounts

import "testing"

// 人 → 花费算哪个账号、声纹名：新账号的本人、访客都算那个账号；本人的声纹名是 account-<编号>（不带冒号）；
// 管理员沿用的名字、全家共用、没标人都算管理员（空），声纹名照原样。
func TestPersonMappings(t *testing.T) {
	owner, guest := PersonsOf("a1")
	for person, want := range map[string]string{owner: "a1", guest: "a1", "主人": "", "_shared": "", "": "", "account:": ""} {
		if got := BillingAccountOf(person); got != want {
			t.Errorf("BillingAccountOf(%q) = %q，应为 %q", person, got, want)
		}
	}
	for person, want := range map[string]string{owner: "account-a1", "主人": "主人", guest: guest} {
		if got := VoiceprintNameOf(person); got != want {
			t.Errorf("VoiceprintNameOf(%q) = %q，应为 %q", person, got, want)
		}
	}
}
