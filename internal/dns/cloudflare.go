// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package dns 的 Cloudflare 实现（M4 T041）。
//
// 调用 Cloudflare API v4（https://api.cloudflare.com/client/v4）：
//   - 凭据仅用 API Token（Bearer），最小权限 Zone:Read + Zone:DNS:Edit，且限定到具体 zone；
//   - 全链经 HTTPS，token 绝不落日志；
//   - BaseURL 可注入，便于 httptest 模拟（见 cloudflare_test.go）。
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// Cloudflare 是 Cloudflare DNS-01 provider。
type Cloudflare struct {
	Token   string
	BaseURL string // 默认 https://api.cloudflare.com/client/v4；测试时可替换为 httptest URL
	Client  *http.Client

	mu    sync.Mutex
	zones map[string]string // zone name -> zone id 缓存，避免每次查询
}

// NewCloudflare 用 API Token 构造 Cloudflare provider。
func NewCloudflare(token string) *Cloudflare {
	return &Cloudflare{
		Token:   token,
		BaseURL: "https://api.cloudflare.com/client/v4",
		Client:  &http.Client{Timeout: 30 * time.Second},
		zones:   make(map[string]string),
	}
}

// Name 返回 provider 标识。
func (c *Cloudflare) Name() string { return "cloudflare" }

// cfResponse 是 CF API 的统一响应外壳。
type cfResponse struct {
	Success  bool       `json:"success"`
	Errors   []cfError  `json:"errors"`
	Result   json.RawMessage `json:"result"`
	ResultInfo struct {
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Validate 校验 token 可读取 zone（最小权限基本满足）。
func (c *Cloudflare) Validate(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/zones?per_page=1", nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "构造校验请求失败", err)
	}
	c.auth(req)
	var out cfResponse
	if err := c.do(req, &out); err != nil {
		return err
	}
	if !out.Success {
		return apperr.New(apperr.CodeUnavailable,
			"Cloudflare 凭据校验失败："+cfErrorMessage(out.Errors))
	}
	return nil
}

// SetRecord 写入/更新 _acme-challenge TXT 记录（幂等）。
func (c *Cloudflare) SetRecord(ctx context.Context, zone, name, value string) error {
	if zone == "" || name == "" {
		return apperr.New(apperr.CodeInvalid, "zone 与 name 均不可为空")
	}
	zid, err := c.resolveZone(ctx, zone)
	if err != nil {
		return err
	}
	existing, err := c.listTXT(ctx, zid, name)
	if err != nil {
		return err
	}
	if len(existing) == 1 {
		// 更新既有记录（内容变更才提交）。
		if existing[0].Content == value {
			return nil
		}
		return c.patchRecord(ctx, zid, existing[0].ID, value)
	}
	if len(existing) > 1 {
		// 多条同名（异常残留）：全部删除后重建，保证幂等。
		for _, r := range existing {
			if err := c.deleteRecord(ctx, zid, r.ID); err != nil {
				return err
			}
		}
	}
	return c.createRecord(ctx, zid, name, value)
}

// DeleteRecord 删除匹配 name 的全部 TXT 记录。
func (c *Cloudflare) DeleteRecord(ctx context.Context, zone, name string) error {
	zid, err := c.resolveZone(ctx, zone)
	if err != nil {
		return err
	}
	existing, err := c.listTXT(ctx, zid, name)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if err := c.deleteRecord(ctx, zid, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// resolveZone 把 zone name 解析为 zone id（带缓存）。
func (c *Cloudflare) resolveZone(ctx context.Context, zone string) (string, error) {
	c.mu.Lock()
	if id, ok := c.zones[zone]; ok {
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/zones?name="+zone+"&per_page=1", nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "构造 zone 查询失败", err)
	}
	c.auth(req)
	var out cfResponse
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if !out.Success || out.ResultInfo.TotalCount == 0 {
		return "", apperr.New(apperr.CodeInvalid,
			"找不到 Cloudflare zone："+zone)
	}
	var zones []cfZone
	if err := json.Unmarshal(out.Result, &zones); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "解析 zone 响应失败", err)
	}
	if len(zones) == 0 {
		return "", apperr.New(apperr.CodeInvalid, "找不到 Cloudflare zone："+zone)
	}
	c.mu.Lock()
	c.zones[zone] = zones[0].ID
	c.mu.Unlock()
	return zones[0].ID, nil
}

// listTXT 列出某 zone 下匹配 name 的全部 TXT 记录。
func (c *Cloudflare) listTXT(ctx context.Context, zid, name string) ([]cfRecord, error) {
	u := fmt.Sprintf("%s/zones/%s/dns_records?type=TXT&name=%s&per_page=100",
		c.baseURL(), zid, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "构造记录查询失败", err)
	}
	c.auth(req)
	var out cfResponse
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, apperr.New(apperr.CodeUnavailable,
			"查询 TXT 记录失败："+cfErrorMessage(out.Errors))
	}
	var recs []cfRecord
	if err := json.Unmarshal(out.Result, &recs); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "解析记录响应失败", err)
	}
	return recs, nil
}

func (c *Cloudflare) createRecord(ctx context.Context, zid, name, value string) error {
	body, _ := json.Marshal(map[string]any{
		"type":    "TXT",
		"name":    name,
		"content": value,
		"ttl":     60,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/zones/%s/dns_records", c.baseURL(), zid), bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "构造创建请求失败", err)
	}
	c.auth(req)
	c.json(req)
	var out cfResponse
	if err := c.do(req, &out); err != nil {
		return err
	}
	if !out.Success {
		return apperr.New(apperr.CodeUnavailable,
			"创建 TXT 记录失败："+cfErrorMessage(out.Errors))
	}
	return nil
}

func (c *Cloudflare) patchRecord(ctx context.Context, zid, rid, value string) error {
	body, _ := json.Marshal(map[string]any{"content": value})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/zones/%s/dns_records/%s", c.baseURL(), zid, rid), bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "构造更新请求失败", err)
	}
	c.auth(req)
	c.json(req)
	var out cfResponse
	if err := c.do(req, &out); err != nil {
		return err
	}
	if !out.Success {
		return apperr.New(apperr.CodeUnavailable,
			"更新 TXT 记录失败："+cfErrorMessage(out.Errors))
	}
	return nil
}

func (c *Cloudflare) deleteRecord(ctx context.Context, zid, rid string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/zones/%s/dns_records/%s", c.baseURL(), zid, rid), nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "构造删除请求失败", err)
	}
	c.auth(req)
	var out cfResponse
	if err := c.do(req, &out); err != nil {
		return err
	}
	if !out.Success {
		return apperr.New(apperr.CodeUnavailable,
			"删除 TXT 记录失败："+cfErrorMessage(out.Errors))
	}
	return nil
}

// do 执行请求并解析 CF 响应外壳；非 2xx 或 success=false 时返回明确错误。
func (c *Cloudflare) do(req *http.Request, out *cfResponse) error {
	resp, err := c.client().Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "Cloudflare API 请求失败", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apperr.New(apperr.CodeUnavailable,
			fmt.Sprintf("Cloudflare API HTTP %d：%s", resp.StatusCode, truncate(string(data), 256)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "解析 Cloudflare 响应失败", err)
	}
	return nil
}

func (c *Cloudflare) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Cloudflare) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.cloudflare.com/client/v4"
}

func (c *Cloudflare) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
}

func (c *Cloudflare) json(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
}

func cfErrorMessage(errs []cfError) string {
	if len(errs) == 0 {
		return "未知错误"
	}
	msg := ""
	for i, e := range errs {
		if i > 0 {
			msg += "；"
		}
		msg += fmt.Sprintf("[%d] %s", e.Code, e.Message)
	}
	return msg
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
