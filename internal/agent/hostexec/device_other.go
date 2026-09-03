// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

//go:build !unix

package hostexec

// deviceID 在非 unix 平台无法获取设备 ID，恒返回 0。
// NGX-CP 的目标环境是 Linux 裸机/VM，此处仅为保持跨平台可编译；
// 返回 0 会让调用方按「无法判定」降级处理（提示人工确认），不会误判为同设备。
func deviceID(string) uint64 { return 0 }
