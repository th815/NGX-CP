package logtail

import (
	"reflect"
	"testing"
)

func TestBatchRoundTrip(t *testing.T) {
	lines := []LogLine{
		{TS: "2026-09-08T10:00:00+08:00", Status: 200, URI: "/a", Node: "rs-09", RemoteAddr: "1.2.3.4"},
		{TS: "2026-09-08T10:00:01+08:00", Status: 503, URI: "/b", Node: "rs-09", RemoteAddr: "1.2.3.5", Raw: "ignored-raw"},
		{TS: "not-a-ts", Status: 0, URI: "/bad", Node: "rs-09", Raw: "this is not json"},
	}
	raw, err := MarshalBatch(lines)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalBatch(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != len(lines) {
		t.Fatalf("round-trip count = %d, want %d", len(got), len(lines))
	}
	// Raw 带 json:"-" 不参与 JSON 往返，构造期望时把 Raw 清空再比较。
	want := make([]LogLine, len(lines))
	for i, l := range lines {
		l.Raw = "" // 还原后 Raw 必为空
		want[i] = l
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("line %d mismatch: got %+v want %+v", i, got[i], want[i])
		}
	}
	// Raw 不参与 JSON 往返（json:"-"），故还原后 Raw 为空，但其余字段保留。
	if got[1].Raw != "" {
		t.Fatalf("raw should be excluded from JSON, got %q", got[1].Raw)
	}
	if got[1].Status != 503 || got[1].URI != "/b" {
		t.Fatalf("structured fields lost: %+v", got[1])
	}
	if got[2].Status != 0 || got[2].URI != "/bad" {
		t.Fatalf("unparseable line should round-trip with zero status: %+v", got[2])
	}
}

func TestUnmarshalBatch_InvalidJSON(t *testing.T) {
	if _, err := UnmarshalBatch([]byte("not json")); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}
