package appserver

import (
	"encoding/json"
	"net/http"

	"github.com/bukahou/haro/internal/appapi"
)

// problemReply 是出错的回答（RFC 9457）：每个接口的响应接口都实现一遍，接口出错时统一回它。
// 下面的 Visit 方法每个接口一个、写法相同（Go 没法让一个类型「对所有接口都算实现」）；契约加了接口就照样加一个。
type problemReply struct {
	problem appapi.Problem
}

func (p problemReply) write(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.problem.Status)
	return json.NewEncoder(w).Encode(p.problem)
}

func (p problemReply) VisitApproveTerminalResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitGetClientConfigResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitGetHomeResponse(w http.ResponseWriter) error         { return p.write(w) }
func (p problemReply) VisitGetMeResponse(w http.ResponseWriter) error           { return p.write(w) }
func (p problemReply) VisitGetOverviewResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p problemReply) VisitGetProfileResponse(w http.ResponseWriter) error      { return p.write(w) }
func (p problemReply) VisitGetSpendingResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p problemReply) VisitListAccountsResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p problemReply) VisitListCountriesResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitListPendingTerminalsResponse(w http.ResponseWriter) error {
	return p.write(w)
}
func (p problemReply) VisitListTerminalsResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p problemReply) VisitLogoutResponse(w http.ResponseWriter) error         { return p.write(w) }
func (p problemReply) VisitPairTerminalResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitRefreshTokensResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p problemReply) VisitRejectTerminalResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitRemoveTerminalResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitRenameTerminalResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitReplaceHomeResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p problemReply) VisitReplaceProfileResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitSearchPlacesResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitUpdateMeResponse(w http.ResponseWriter) error       { return p.write(w) }

func (p problemReply) VisitListConversationsResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p problemReply) VisitCreateConversationResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitDeleteConversationResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitRestoreConversationResponse(w http.ResponseWriter) error {
	return p.write(w)
}
func (p problemReply) VisitListTurnsResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitStopTurnResponse(w http.ResponseWriter) error  { return p.write(w) }

func (p problemReply) VisitSearchTurnsResponse(w http.ResponseWriter) error { return p.write(w) }

func (p problemReply) VisitGetBackstoryResponse(w http.ResponseWriter) error      { return p.write(w) }
func (p problemReply) VisitUpdateBackstoryResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitEditBackstoryItemResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitDeleteBackstoryItemResponse(w http.ResponseWriter) error {
	return p.write(w)
}
func (p problemReply) VisitListNotesResponse(w http.ResponseWriter) error         { return p.write(w) }
func (p problemReply) VisitForgetNoteResponse(w http.ResponseWriter) error        { return p.write(w) }
func (p problemReply) VisitRestoreNoteResponse(w http.ResponseWriter) error       { return p.write(w) }
func (p problemReply) VisitListResearchTasksResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitGetResearchTaskResponse(w http.ResponseWriter) error   { return p.write(w) }

func (p problemReply) VisitListOccurrencesResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitCreateEventResponse(w http.ResponseWriter) error       { return p.write(w) }
func (p problemReply) VisitGetEventResponse(w http.ResponseWriter) error          { return p.write(w) }
func (p problemReply) VisitUpdateEventResponse(w http.ResponseWriter) error       { return p.write(w) }
func (p problemReply) VisitDeleteEventResponse(w http.ResponseWriter) error       { return p.write(w) }
func (p problemReply) VisitListPhoneAlertsResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitListCalendarSyncsResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitSyncCalendarResponse(w http.ResponseWriter) error      { return p.write(w) }

func (p problemReply) VisitGetReceiptResponse(w http.ResponseWriter) error        { return p.write(w) }
func (p problemReply) VisitDiscardReceiptResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p problemReply) VisitConfirmReceiptResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p problemReply) VisitListExpensesResponse(w http.ResponseWriter) error      { return p.write(w) }
func (p problemReply) VisitCreateExpenseResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p problemReply) VisitGetExpenseResponse(w http.ResponseWriter) error        { return p.write(w) }
func (p problemReply) VisitUpdateExpenseResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p problemReply) VisitDeleteExpenseResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p problemReply) VisitSummarizeExpensesResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitListExpenseCategoriesResponse(w http.ResponseWriter) error {
	return p.write(w)
}
func (p problemReply) VisitAddExpenseCategoryResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitDeleteExpenseCategoryResponse(w http.ResponseWriter) error {
	return p.write(w)
}

func (p problemReply) VisitApproveAccountResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitDisableAccountResponse(w http.ResponseWriter) error { return p.write(w) }
func (p problemReply) VisitEnableAccountResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p problemReply) VisitDeleteAccountResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p problemReply) VisitListInvitesResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p problemReply) VisitCreateInviteResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p problemReply) VisitRevokeInviteResponse(w http.ResponseWriter) error   { return p.write(w) }
