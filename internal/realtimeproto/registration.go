package realtimeproto

// 终端登记接口（haro-server 提供，照 RFC 8628 设备授权的样子）：终端拿到配对码给人看（人在 App 里输入，或者等管理员批准），
// 自己拿轮询凭据定时问结果，批准了就拿到设备凭证存下来。之后连语音会话时把设备凭证放在握手的 Authorization: Bearer 里。
// 请求和响应的结构只在这里定义，两端共用。
//
//	POST RegistrationPath      请求体 RegistrationRequest；成功 200 + RegistrationStarted
//	POST RegistrationPollPath  请求体 RegistrationPoll；成功 200 + RegistrationOutcome；轮询凭据不认得 404（CodeRegistrationNotFound）
//	                           问得太勤 429（CodeRateLimited）：按 RegistrationStarted.Interval 来

// RegistrationPath 是开始登记的路径。
const RegistrationPath = "/v1/terminal-registration"

// RegistrationRoute 是注册路由用的写法。
const RegistrationRoute = "POST " + RegistrationPath

// RegistrationPollPath 是问登记结果的路径。
const RegistrationPollPath = "/v1/terminal-registration/poll"

// RegistrationPollRoute 是注册路由用的写法。
const RegistrationPollRoute = "POST " + RegistrationPollPath

// MaxTerminalNameRunes 是终端报的名字最多几个字（超过的截掉）。
const MaxTerminalNameRunes = 40

// RegistrationRequest 是开始登记：终端报自己的名字（默认主机名，App 里能改）。版本在 buildinfo 的请求头里。
type RegistrationRequest struct {
	Name string `json:"name"`
}

// RegistrationStarted 是登记开始了：Code 给人看（App 里输入），PollToken 终端自己留着问结果。
type RegistrationStarted struct {
	Code      string `json:"code"`
	ShortID   string `json:"short_id"` // 短编号：管理员在 App 的「新终端」里看到的同一个编号（等批准时对得上）
	PollToken string `json:"poll_token"`
	ExpiresIn int    `json:"expires_in"` // 配对码几秒内有效
	Interval  int    `json:"interval"`   // 至少隔几秒问一次
}

// RegistrationPoll 是问登记结果。
type RegistrationPoll struct {
	PollToken string `json:"poll_token"`
}

// RegistrationStatus 是登记的结果。
type RegistrationStatus string

// 登记的结果。
const (
	RegistrationPending  RegistrationStatus = "pending"  // 还在等：过 Interval 秒再问
	RegistrationApproved RegistrationStatus = "approved" // 批准了：带着设备凭证（只给这一次）
	RegistrationRejected RegistrationStatus = "rejected" // 管理员拒绝了
	RegistrationExpired  RegistrationStatus = "expired"  // 配对码过期了：重新登记
)

// RegistrationOutcome 是问到的结果。
type RegistrationOutcome struct {
	Status     RegistrationStatus `json:"status"`
	TerminalID string             `json:"terminal_id,omitempty"`
	Credential string             `json:"credential,omitempty"`
}
