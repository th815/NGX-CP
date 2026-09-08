package agentv1

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestHeartbeatRequest_LogBatchRoundtrip 验证 T063 离线 patch 的 rawDesc：
// HeartbeatRequest 的 log_batch 字段（field 14, bytes）与 LOG_BATCH 枚举值（12）
// 在运行时描述符中真实存在，且可经线格式正确序列化/反序列化。
func TestHeartbeatRequest_LogBatchRoundtrip(t *testing.T) {
	// 枚举值存在性。
	if HeartbeatRequest_LOG_BATCH != HeartbeatRequest_Type(12) {
		t.Fatalf("LOG_BATCH enum value = %d, want 12", HeartbeatRequest_LOG_BATCH)
	}
	if HeartbeatRequest_Type_name[12] != "LOG_BATCH" {
		t.Fatalf("LOG_BATCH name missing from descriptor map, got %q", HeartbeatRequest_Type_name[12])
	}
	if HeartbeatRequest_Type_value["LOG_BATCH"] != 12 {
		t.Fatalf("LOG_BATCH value missing from descriptor map")
	}

	// 字段存在性（运行时反射）。
	md := (&HeartbeatRequest{}).ProtoReflect().Descriptor()
	fd := md.Fields().ByName("log_batch")
	if fd == nil {
		t.Fatalf("field log_batch not present in runtime descriptor")
	}
	if fd.Number() != 14 {
		t.Fatalf("log_batch field number = %d, want 14", fd.Number())
	}
	if fd.Kind().String() != "bytes" {
		t.Fatalf("log_batch field kind = %s, want bytes", fd.Kind())
	}

	// 线格式往返。
	payload := []byte(`{"lines":[{"time":"2026-09-08T10:00:00+08:00","status":200,"uri":"/x"}]}`)
	in := &HeartbeatRequest{
		Type:      HeartbeatRequest_LOG_BATCH,
		Timestamp: 1700000000,
		LogBatch:  payload,
	}
	buf, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 确认线上字节包含字段 14 的 wire tag：field_number 14, wire type 2 (length-delimited) => (14<<3)|2 = 114 = 0x72。
	found := false
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] == 0x72 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("log_batch (field 14, wire tag 0x72) not present on the wire: %x", buf)
	}

	out := &HeartbeatRequest{}
	if err := proto.Unmarshal(buf, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.GetType() != HeartbeatRequest_LOG_BATCH {
		t.Fatalf("type = %v, want LOG_BATCH", out.GetType())
	}
	if string(out.GetLogBatch()) != string(payload) {
		t.Fatalf("log_batch roundtrip mismatch: %q vs %q", out.GetLogBatch(), payload)
	}
}
