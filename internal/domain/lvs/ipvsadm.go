// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package lvs 实现 LVS 权重编排的领域逻辑（T035）：解析 ipvsadm 输出、在 RS 的
// 所有 VS 条目上批量置权重（摘除/加回）、以及灰度发布编排（见 graceful.go）。
//
// 设计要点：
//   - 权重操作按「RS + 其所在的每一个 VS」进行。生产环境一个 RS 往往同时挂在
//     :80 / :443(tcp) / :443(udp) 多条 VS 上，灰度必须把这三条的权重同时置 0，
//     否则只摘 :80 会让 443 流量继续打向该 RS，灰度不彻底（已在生产环境核实）。
//   - 所有真实命令经 hostexec.CommandExecutor 抽象，单测用 fake 注入，无需 root。
package lvs

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/th/ngxcp/internal/agent/hostexec"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// VirtualServerRef 唯一定位一个虚拟服务（协议 + 地址 + 端口）。
type VirtualServerRef struct {
	Proto   string // TCP / UDP
	Address string // 如 192.168.5.5
	Port    int
}

// Key 返回稳定可比较的键（用于 map 索引）。
func (v VirtualServerRef) Key() string { return fmt.Sprintf("%s|%s|%d", v.Proto, v.Address, v.Port) }

// String 供日志/报错使用。
func (v VirtualServerRef) String() string { return fmt.Sprintf("%s %s:%d", v.Proto, v.Address, v.Port) }

// RealServerRef 唯一定位一个真实服务器（地址 + 端口）。
type RealServerRef struct {
	Address string
	Port    int
}

// Key 返回稳定可比较的键。
func (r RealServerRef) Key() string { return fmt.Sprintf("%s|%d", r.Address, r.Port) }

// String 供日志/报错使用。
func (r RealServerRef) String() string { return fmt.Sprintf("%s:%d", r.Address, r.Port) }

// RealServer 是解析 ipvsadm 得到的一条 RS 记录。
type RealServer struct {
	Ref          RealServerRef
	Forward      string // Route / Masq / Tunnel
	Weight       int
	ActiveConn   int
	InactiveConn int
}

// VirtualServer 是解析 ipvsadm 得到的一条 VS 记录。
type VirtualServer struct {
	Ref         VirtualServerRef
	Scheduler   string
	Flags       string // 如 "persistent 60"
	RealServers []RealServer
}

// WeightSetter 抽象「设置 RS 在某 VS 上的权重」与「列出当前所有 VS」的能力。
// Agent 侧的 ipvs.IPVSExecutor / lvs.RealWeightSetter 满足本接口。
type WeightSetter interface {
	// SetWeight 将 rs 在 vs 上的权重设为 weight（0 = 摘除）。
	SetWeight(ctx context.Context, vs VirtualServerRef, rs RealServerRef, weight int) error
	// ListVirtualServers 返回当前所有 VS（含 RS 与权重），用于快照原权重与排空检测。
	ListVirtualServers(ctx context.Context) ([]VirtualServer, error)
}

// RealWeightSetter 用真实 ipvsadm 命令实现 WeightSetter（运行于 LVS Director 节点）。
type RealWeightSetter struct {
	Exec hostexec.CommandExecutor
	Bin  string // ipvsadm 路径，默认 /sbin/ipvsadm
}

func (w *RealWeightSetter) bin() string {
	if w.Bin != "" {
		return w.Bin
	}
	return "/sbin/ipvsadm"
}

// ListVirtualServers 执行 `ipvsadm -Ln` 并解析。
func (w *RealWeightSetter) ListVirtualServers(ctx context.Context) ([]VirtualServer, error) {
	out, err := w.Exec.Output(ctx, w.bin(), "-Ln")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnavailable, "执行 ipvsadm -Ln 失败: "+out, err)
	}
	return ParseIPVS(out)
}

// SetWeight 执行 `ipvsadm -e -t|-u <VIP>:<port> -r <RS>:<port> -w <weight>`。
func (w *RealWeightSetter) SetWeight(ctx context.Context, vs VirtualServerRef, rs RealServerRef, weight int) error {
	protoFlag := "-t"
	if strings.EqualFold(vs.Proto, "UDP") {
		protoFlag = "-u"
	}
	_, err := w.Exec.Output(ctx, w.bin(), "-e", protoFlag,
		fmt.Sprintf("%s:%d", vs.Address, vs.Port),
		"-r", fmt.Sprintf("%s:%d", rs.Address, rs.Port),
		"-w", strconv.Itoa(weight),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("ipvsadm 置权重失败 %s -> %s w=%d", vs, rs, weight), err)
	}
	return nil
}

// ParseIPVS 解析 `ipvsadm -Ln` 的文本输出为结构化 VS/RS 列表。
func ParseIPVS(out string) ([]VirtualServer, error) {
	var vss []VirtualServer
	var cur *VirtualServer
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "IP Virtual Server") ||
			strings.HasPrefix(line, "Prot LocalAddress") {
			continue
		}
		if strings.HasPrefix(line, "->") {
			if cur == nil {
				continue
			}
			rs, err := parseRealServer(line)
			if err != nil {
				return nil, err
			}
			cur.RealServers = append(cur.RealServers, rs)
			continue
		}
		vs, err := parseVirtualServer(line)
		if err != nil {
			return nil, err
		}
		vss = append(vss, vs)
		cur = &vss[len(vss)-1]
	}
	return vss, nil
}

func parseVirtualServer(line string) (VirtualServer, error) {
	f := strings.Fields(line)
	if len(f) < 3 {
		return VirtualServer{}, apperr.New(apperr.CodeInvalid, "无法解析 VS 行: "+line)
	}
	proto := strings.ToUpper(f[0])
	if proto != "TCP" && proto != "UDP" {
		return VirtualServer{}, apperr.New(apperr.CodeInvalid, "未知 VS 协议: "+f[0])
	}
	addr, port, err := splitHostPort(f[1])
	if err != nil {
		return VirtualServer{}, err
	}
	vs := VirtualServer{
		Ref:       VirtualServerRef{Proto: proto, Address: addr, Port: port},
		Scheduler: f[2],
	}
	if len(f) > 3 {
		vs.Flags = strings.Join(f[3:], " ")
	}
	return vs, nil
}

func parseRealServer(line string) (RealServer, error) {
	f := strings.Fields(line)
	if len(f) < 6 {
		return RealServer{}, apperr.New(apperr.CodeInvalid, "无法解析 RS 行: "+line)
	}
	addr, port, err := splitHostPort(f[1])
	if err != nil {
		return RealServer{}, err
	}
	weight, err := strconv.Atoi(f[3])
	if err != nil {
		return RealServer{}, apperr.Wrap(apperr.CodeInvalid, "权重解析失败 "+f[3], err)
	}
	active, err := strconv.Atoi(f[4])
	if err != nil {
		return RealServer{}, apperr.Wrap(apperr.CodeInvalid, "ActiveConn 解析失败 "+f[4], err)
	}
	inactive, _ := strconv.Atoi(f[5])
	return RealServer{
		Ref:          RealServerRef{Address: addr, Port: port},
		Forward:      f[2],
		Weight:       weight,
		ActiveConn:   active,
		InactiveConn: inactive,
	}, nil
}

func splitHostPort(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, apperr.New(apperr.CodeInvalid, "缺少端口: "+s)
	}
	host, portStr := s[:i], s[i+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInvalid, "端口非数字 "+portStr, err)
	}
	return host, port, nil
}

// VirtualServersForRS 返回包含指定 RS 的所有 VS 条目（用于批量置权重）。
func VirtualServersForRS(vss []VirtualServer, rs RealServerRef) []VirtualServer {
	var out []VirtualServer
	for _, vs := range vss {
		for _, r := range vs.RealServers {
			if r.Ref == rs {
				out = append(out, vs)
				break
			}
		}
	}
	return out
}

// SnapshotWeights 抓取指定 RS 在当前所有 VS 上的原权重，便于灰度结束后精确加回。
func SnapshotWeights(vss []VirtualServer, rs RealServerRef) map[VirtualServerRef]int {
	m := map[VirtualServerRef]int{}
	for _, vs := range vss {
		for _, r := range vs.RealServers {
			if r.Ref == rs {
				m[vs.Ref] = r.Weight
				break
			}
		}
	}
	return m
}

// activeOf 统计某 RS 在所有 VS 上的 ActiveConn 之和（排空判定用）。
func activeOf(vss []VirtualServer, rs RealServerRef) int {
	n := 0
	for _, vs := range vss {
		for _, r := range vs.RealServers {
			if r.Ref == rs {
				n += r.ActiveConn
			}
		}
	}
	return n
}
