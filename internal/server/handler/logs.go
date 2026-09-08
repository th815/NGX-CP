package handler

import (
	"net/http"
	"sort"
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

// Trace 处理 TraceID 全链路追踪（T064）。
//
//	GET /api/v1/logs/trace/:request_id
//	→ { code, data:{ rid, spans:[Entry ordered by ts ASC], nodes, first_hop, bottleneck, took_ms } }
//
// 同一次请求经 LVS/反向代理跨节点时，各节点以同一 $request_id（rid）记录日志；
// 本接口按 rid 聚合全部 span，按时间升序还原链路，标出首跳节点与瓶颈节点
// （upstream_rt 最大者），供运维定位"慢在哪一跳 / 落在哪个后端"。
func (h *LogsHandler) Trace(c *gin.Context) {
	rid := c.Param("request_id")
	if rid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "request_id 不能为空"})
		return
	}

	// 取该 rid 的全部 span（单请求跨节点通常 1–3 条，Size 500 足够覆盖）。
	res, err := h.store.Query(c.Request.Context(), logstore.QueryParams{
		RID:   rid,
		Size:  500,
		Page:  1,
		Regex: false,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	// 链路视角：时间升序（从首跳到末跳）。
	spans := make([]logstore.Entry, len(res.Items))
	copy(spans, res.Items)
	sort.SliceStable(spans, func(i, j int) bool {
		return spans[i].TS.Before(spans[j].TS)
	})

	nodes := make([]string, 0, len(spans))
	seen := make(map[string]struct{}, len(spans))
	firstHop := ""
	bottleneck := ""
	var bottleneckRT float32
	for i, s := range spans {
		if _, ok := seen[s.Node]; !ok {
			seen[s.Node] = struct{}{}
			nodes = append(nodes, s.Node)
		}
		if i == 0 {
			firstHop = s.Node
		}
		if s.UpstreamRT > bottleneckRT {
			bottleneckRT = s.UpstreamRT
			bottleneck = s.Node
		}
	}

	response.OK(c, gin.H{
		"rid":        rid,
		"spans":      spans,
		"nodes":      nodes,
		"first_hop":  firstHop,
		"bottleneck": bottleneck,
		"took_ms":    res.TookMs,
	})
}
