package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// NodeConfigFile 是 `nginx -T` 解析出的单个配置文件（T017 产出，T018 落库）。
//
// 只持久化**元数据**，不存内容：
//   - 完整配置常达数 MB，常驻库内既浪费存储，也会把 ssl_certificate_key 等敏感路径
//     连带扩散到数据库（Agent 端本就刻意不读取私钥内容，见 T017 陷阱）；
//   - 前端「配置树」Tab 默认只列文件（路径/大小/哈希），点击才向 Agent 请求内容。
//
// path + node 唯一：同一节点的配置树每次采集整体替换（配置树是快照语义，不做增量）。
type NodeConfigFile struct {
	ent.Schema
}

func (NodeConfigFile) Fields() []ent.Field {
	return []ent.Field{
		field.String("path"),      // /etc/nginx/nginx.conf
		field.String("sha256"),    // 内容哈希（去首尾空行后的原文）
		field.Int64("size"),       // 字节
		field.Time("mod_time").Optional(), // 文件 mtime（Agent stat 得到，取不到则为空）
		field.Time("captured_at"),         // 本次采集时间
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (NodeConfigFile) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("node", Node.Type).
			Ref("config_files").
			Unique().
			Required(),
	}
}

func (NodeConfigFile) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("path").
			Edges("node").
			Unique(),
	}
}
