package brain

import "context"

// billingKey 是 ctx 里「这次的花费算谁的」（人；记账时由花费账本换成账号）。
type billingKey struct{}

// WithBillingPerson 标明这次（这一轮、这个后台任务）的模型调用、搜索算 person 的：记账时按人找到账号，按账号的月上限判断。
// 会话每一轮按说话的人标；后台的事（调查、背景记录池、起标题、读小票、滚动摘要）按任务所属的人标。
func WithBillingPerson(ctx context.Context, person string) context.Context {
	return context.WithValue(ctx, billingKey{}, person)
}

// BillingPersonFrom 是 ctx 里标的人；没标时为空（算服务端主人的）。
func BillingPersonFrom(ctx context.Context) string {
	person, _ := ctx.Value(billingKey{}).(string)
	return person
}
