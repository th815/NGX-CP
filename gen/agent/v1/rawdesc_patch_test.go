package agentv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestComplianceReport_HoldingVipRoundtrip 验证：手动给 rawDesc 加的 holding_vip
// 字段（field 5）能真正经 protobuf 序列化/反序列化往返，证明离线 patch 的
// descriptor 与 struct tag 一致（否则该字段会被当作 unknown 字段丢弃）。
func TestComplianceReport_HoldingVipRoundtrip(t *testing.T) {
	in := &ComplianceReport{
		CheckedAt:  1700000000,
		Role:       "director",
		Passed:     true,
		HoldingVip: true,
	}
	buf, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 确认线上字节确实包含字段 5（wire tag = (5<<3)|0 = 0x28）。
	found := false
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] == 0x28 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("holding_vip (field 5, wire tag 0x28) not present on the wire: %x", buf)
	}

	out := &ComplianceReport{}
	if err := proto.Unmarshal(buf, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.GetHoldingVip() {
		t.Fatalf("holding_vip lost in roundtrip: %+v", out)
	}
	if out.GetRole() != "director" || !out.GetPassed() {
		t.Fatalf("other fields corrupted in roundtrip: %+v", out)
	}

	// 反向：未设置的 holding_vip 应解回 false，不污染其他字段。
	in2 := &ComplianceReport{Role: "real_server", Passed: true}
	b2, _ := proto.Marshal(in2)
	out2 := &ComplianceReport{}
	if err := proto.Unmarshal(b2, out2); err != nil {
		t.Fatalf("unmarshal2: %v", err)
	}
	if out2.GetHoldingVip() {
		t.Fatalf("holding_vip should default false, got true")
	}
}
