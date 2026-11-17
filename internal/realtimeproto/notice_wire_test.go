package realtimeproto

import "testing"

func TestNoticeRoundTrip(t *testing.T) {
	data, err := EncodeServerEvent(HaroNotice{Kind: NoticeResearchDone})
	if err != nil {
		t.Fatal(err)
	}
	event, err := DecodeServerWireEvent(data)
	if err != nil || event != (HaroNotice{Kind: NoticeResearchDone}) {
		t.Fatalf("编解码：%s → %#v %v", data, event, err)
	}
}
