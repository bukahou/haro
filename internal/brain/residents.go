package brain

// Residents 是这台终端所属账号的两个「人」（对话、记忆、提醒等存储按人存）：认出是本人的声音时用本人，
// 别的声音（登记过的别人）用访客 —— 一台终端只认它所属账号的本人。
// 零值 = 不分账号（管理员账号的终端、没带设备凭证的终端）：声纹认出的名字照用，还没认出时用全家共用（SharedPerson）。
type Residents struct {
	Owner string // 账号本人：声纹登记时用的就是这个名字
	Guest string // 账号的访客
}

// separate 判断分不分账号。
func (r Residents) separate() bool { return r.Guest != "" }

// shared 是还没认出是谁时用的人：分账号时是访客，否则是全家共用。
func (r Residents) shared() string {
	if r.separate() {
		return r.Guest
	}
	return SharedPerson
}

// personFor 是声纹认出的名字在这台终端上算谁。
func (r Residents) personFor(speaker string) string {
	switch {
	case !r.separate():
		return speaker
	case speaker == r.Owner:
		return r.Owner
	}
	return r.Guest
}

// verified 判断这句的声纹算不算「声纹确认」（确认闸只让这样的人发起、确认会破坏的操作）：分账号时只有本人算。
func (r Residents) verified(speaker string) bool {
	return speaker != "" && (!r.separate() || speaker == r.Owner)
}

// speakerLine 是告诉模型现在谁在说话的那一行；访客、全家共用时没有。
func (r Residents) speakerLine(person string) string {
	switch {
	case person == "" || person == SharedPerson || (r.separate() && person == r.Guest):
		return ""
	case r.separate():
		return "现在说话的是这台终端的主人（按声纹认出）。"
	}
	return "现在说话的是：" + person + "（按声纹认出）。"
}

// reported 是告诉终端这句是谁说的：分账号时别人的名字换成访客；没认出（空）照旧是空。
func (r Residents) reported(speaker string) string {
	if speaker == "" {
		return ""
	}
	return r.personFor(speaker)
}
