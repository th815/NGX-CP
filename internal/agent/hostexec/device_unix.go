// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

//go:build unix

package hostexec

import (
	"os"
	"syscall"
)

// deviceID 返回路径所在设备的 ID（st_dev），用于判定两个目录是否位于同一文件系统。
//
// 这是「原子落盘」可行性的判定依据：只有同一设备内的 rename 才是原子的，
// 跨设备 rename 会退化为 copy。取不到时返回 0（调用方应据此降级，而非判定为同设备）。
func deviceID(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Dev)
}
