package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/th/ngxcp/internal/logstore"
	"github.com/th/ngxcp/internal/server/response"
)

// LogsHandler 暴露日志检索 API（T063）。
// 检索依赖 logstore.Storage：生产为 ClickHouseStorage，开发/未配置 DSN 时为 MemStorage。
type LogsHandler struct {
	store logstore.Storage
}

// NewLogsHandler 构造日志处理器。
func NewLogsHandler(store logstore.Storage) *LogsHandler {
	return &LogsHandler{store: store}
}

// logsSearchReq 是 POST /api/v1/logs/search 的请求体（字段与 T063 契约对齐）。
type logsSearchReq struct {
	TimeFrom string   `json:"time_from"` // RFC3339，可空（缺省近 24h）
	TimeTo   string   `json:"time_to"`   // RFC3339，可空
	Nodes    []string `json:"nodes"`     // 节点名列表
	Status   []uint16 `json:"status"`    // HTTP 状态码列表
	URI      string   `json:"uri"`       // 请求路径
	IP       string   `json:"ip"`        // 客户端地址
	RID      string   `json:"rid"`       // TraceID / request_id
	RTMin    float32  `json:"rt_min"`    // request_rt 下限（秒）
	Regex    bool     `json:"regex"`     // URI 是否按正则匹配
	Page     int      `json:"page"`      // 从 1 起，默认 1
	Size     int      `json:"size"`      // 每页条数，默认 50，上限 200
}

// Search 处理多维日志检索。
//
//	POST /api/v1/logs/search
//	→ { code, message, data:{ items:[Entry], total, took_ms } }
func (h *LogsHandler) Search(c *gin.Context) {
	var req logsSearchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "请求体格式错误", "detail": err.Error()})
		return
	}

	p := logstore.QueryParams{
		Nodes:  req.Nodes,
		Status: req.Status,
		URI:    req.URI,
		IP:     req.IP,
		RID:    req.RID,
		RTMin:  req.RTMin,
		Regex:  req.Regex,
		Page:   req.Page,
		Size:   req.Size,
	}
	if req.TimeFrom != "" {
		if t, err := time.Parse(time.RFC3339, req.TimeFrom); err == nil {
			p.TimeFrom = t
		}
	}
	if req.TimeTo != "" {
		if t, err := time.Parse(time.RFC3339, req.TimeTo); err == nil {
			p.TimeTo = t
		}
	}

	res, err := h.store.Query(c.Request.Context(), p)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"items":   res.Items,
		"total":   res.Total,
		"took_ms": res.TookMs,
	})
}
