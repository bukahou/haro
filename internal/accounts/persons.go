package accounts

import "strings"

// 账号下面的两个「人」（对话、记忆、提醒等存储按人存）：新账号是「account:<编号>」和「account:<编号>:guest」；
// 管理员账号沿用原来的声纹名和全家共用（见 EnsureAdmin）。写法只在这里定义。
const (
	personPrefix = "account:"
	guestSuffix  = ":guest"
)

// PersonsOf 是新账号的本人、访客两个人。
func PersonsOf(accountID string) (owner, guest string) {
	return personPrefix + accountID, personPrefix + accountID + guestSuffix
}

// ParsePerson 认出 PersonsOf 写出来的人：哪个账号、是不是本人。管理员账号沿用的名字、终端上声纹认出的名字不是这种写法，ok = false。
func ParsePerson(person string) (accountID string, owner, ok bool) {
	rest, found := strings.CutPrefix(person, personPrefix)
	if !found || rest == "" {
		return "", false, false
	}
	if id, guest := strings.CutSuffix(rest, guestSuffix); guest {
		return id, false, id != "" && !strings.Contains(id, ":")
	}
	return rest, true, !strings.Contains(rest, ":")
}

// BillingAccountOf 是花费算哪个账号的：PersonsOf 写出来的人是那个账号；别的（管理员沿用的名字、全家共用、终端上声纹认出的名字、
// 没标人）都是管理员的，记成空（spending.AccountOf）。
func BillingAccountOf(person string) string {
	if id, _, ok := ParsePerson(person); ok {
		return id
	}
	return ""
}

// voiceprintPrefix 是别的账号的本人登记声纹时的名字开头（声纹的名字做文件名，不收冒号）。
const voiceprintPrefix = "account-"

// VoiceprintNameOf 是这个人登记声纹、被认出时用的名字：PersonsOf 写出来的本人是「account-<编号>」（访客不登记声纹）；
// 别的（管理员沿用的声纹名）照原样。
func VoiceprintNameOf(person string) string {
	if id, owner, ok := ParsePerson(person); ok && owner {
		return voiceprintPrefix + id
	}
	return person
}
