package realtimeproto

// 声纹登记接口（haro-asr 提供的 HTTP 接口，haro-server 原样转发）：终端先取要读的句子，录几句话发过去，
// haro-asr 算向量、存进声纹目录、立刻生效。请求和响应的结构只在这里定义，两端共用。
//
//	GET  EnrollmentScriptPath      成功 200 + EnrollmentScript（第一句以 haro-asr 关键词文件里的唤醒词开头）
//	POST VoiceprintPath + {名字}   请求体 VoiceprintEnrollment；成功 200 + VoiceprintEnrollmentResult，失败 4xx/5xx + HTTPError

// VoiceprintPath 是声纹登记接口的路径前缀，后面接要登记的名字（路径转义）。
const VoiceprintPath = "/v1/voiceprints/"

// VoiceprintRoute 是服务端注册路由用的写法（Go ServeMux 的模式）。
const VoiceprintRoute = "POST " + VoiceprintPath + "{name}"

// VoiceprintDeleteRoute 是删一份声纹（只在 haro-asr 上、只给同一个 Pod 里的服务端用：彻底删账号时；服务端不往外转）。成功 204。
const VoiceprintDeleteRoute = "DELETE " + VoiceprintPath + "{name}"

// EnrollmentScriptPath 是登记要读的句子（GET）。句子由 haro-asr 按它认的唤醒词生成：唤醒词只在它的关键词文件里写一处。
const EnrollmentScriptPath = "/v1/voiceprint-enrollment/sentences"

// EnrollmentScriptRoute 是注册路由用的写法。
const EnrollmentScriptRoute = "GET " + EnrollmentScriptPath

// EnrollmentScript 是登记时要读的句子（给人朗读的文本）。
type EnrollmentScript struct {
	Sentences []string `json:"sentences"`
}

// VoiceprintEnrollment 是登记请求。
type VoiceprintEnrollment struct {
	// Append 必填：true = 补录（加进这个人原来的声纹），false = 重新登记（覆盖原来的）。
	// 没带（或 null）时拒绝（400），不按覆盖处理：漏了这个字段不能把这个人原来的声纹整份换掉
	Append *bool    `json:"append"`
	Clips  []string `json:"clips"` // 每句一段 base64 的 PCM（TranscriptionFormat：16 kHz 单声道）
}

// VoiceprintEnrollmentResult 是登记结果（给人看的诊断数字）。
type VoiceprintEnrollmentResult struct {
	Name           string           `json:"name"`
	Sentences      int              `json:"sentences"`                  // 存下后一共几句
	Pairwise       []VoiceprintPair `json:"pairwise"`                   // 这次几句之间的相似度
	BestVsExisting *float64         `json:"best_vs_existing,omitempty"` // 补录时：新录的和原来的最高相似度
	Speakers       []string         `json:"speakers"`                   // 登记后一共认得哪些人
}

// VoiceprintPair 是两句之间的相似度（句子从 1 数起）。
type VoiceprintPair struct {
	A     int     `json:"a"`
	B     int     `json:"b"`
	Score float64 `json:"score"`
}
