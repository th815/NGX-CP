// Package logtail 实现 Agent 侧 Nginx 访问日志采集模块。
package logtail

import "encoding/json"

// LogBatchPayload 是 Agent 经心跳上报的日志批次线格式。
// 整个批次 JSON 编码后写入 HeartbeatRequest.log_batch 字节字段（T063 传输契约）。
// Lines 直接复用 LogLine：Raw 字段带 json:"-" 故不会进入 JSON，
// 控制面接收后由结构化字段重建 Entry，无需依赖原文字节流。
type LogBatchPayload struct {
	Lines []LogLine `json:"lines"`
}

// MarshalBatch 把一批日志行序列化为 JSON 字节（用于心跳 log_batch 字段）。
func MarshalBatch(lines []LogLine) ([]byte, error) {
	return json.Marshal(LogBatchPayload{Lines: lines})
}

// UnmarshalBatch 从心跳 log_batch 字节还原日志行批次。
func UnmarshalBatch(b []byte) ([]LogLine, error) {
	var p LogBatchPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return p.Lines, nil
}
