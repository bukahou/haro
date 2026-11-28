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
