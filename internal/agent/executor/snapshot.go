// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 实现 Agent 在受管主机上执行的能力型命令（T024 nginx -t / T031 快照）。
//
// SnapshotExecutor 用纯 Go 的 archive/tar + compress/gzip 生成配置快照，
// 不依赖外部 tar 二进制；并在归档内嵌一份元数据（mode/owner/sha256），
// 使快照自描述，恢复时可原样还原权限与属主（否则 nginx 可能读不了配置）。
package executor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/th/ngxcp/internal/domain/backup"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// metaEntryName 是归档内嵌的元数据文件名（恢复时读取后跳过落盘）。
const metaEntryName = ".__ngxcp_snapshot_meta.json"

// SnapshotRequest 是一次快照请求（纯数据，便于单测注入）。
type SnapshotRequest struct {
	Paths         []string // 要快照的目录/文件绝对路径，如 /etc/nginx
	IncludeSSL    bool     // 是否包含各根下的 ssl 子目录（默认 false）
	StagingDir    string   // tar 生成目录（应与目标分区同盘，避免跨盘移动）；空则默认 /var/lib/ngxcp/staging
	NodeID        int
	ChangeOrderID *int
	Type          string // pre_deploy | manual | scheduled
}

// SnapshotExecutor 在节点上创建/恢复配置快照。
type SnapshotExecutor struct{}

// NewSnapshotExecutor 构造快照执行器。
func NewSnapshotExecutor() *SnapshotExecutor { return &SnapshotExecutor{} }

// Create 生成快照 tar.gz 到 StagingDir，返回元信息。
// 流程：遍历路径采集元数据 → 写临时 tar → 内嵌 meta → rename 为最终文件（避免半成品被误用）。
func (e *SnapshotExecutor) Create(ctx context.Context, req SnapshotRequest) (*backup.ConfigSnapshot, error) {
	if len(req.Paths) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "快照请求缺少路径")
	}
	staging := req.StagingDir
	if staging == "" {
		staging = "/var/lib/ngxcp/staging"
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建 staging 目录失败", err)
	}

	files, err := e.walkPaths(req.Paths, req.IncludeSSL)
	if err != nil {
		return nil, err
	}

	ts := time.Now().UTC().Format("20060102T150405")
	finalPath := filepath.Join(staging, fmt.Sprintf("snapshot-%s.tar.gz", ts))
	tmpPath := finalPath + ".tmp"
	if err := e.writeTar(tmpPath, files); err != nil {
		return nil, err
	}
	// staging 内生成再 rename：保证控制面/Agent 看到的要么是完整文件，要么不存在。
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "快照文件落盘失败", err)
	}
	fi, err := os.Stat(finalPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "读取快照文件失败", err)
	}
	return &backup.ConfigSnapshot{
		NodeID:        req.NodeID,
		ChangeOrderID: req.ChangeOrderID,
		Type:          req.Type,
		Path:          finalPath,
		Files:         files,
		Size:          fi.Size(),
		CreatedAt:     time.Now().UTC(),
	}, nil
}

// walkPaths 遍历各根目录采集文件元数据，includeSSL=false 时跳过各根下的 ssl 子目录。
func (e *SnapshotExecutor) walkPaths(roots []string, includeSSL bool) ([]backup.SnapshotFile, error) {
	sslDirs := map[string]bool{}
	for _, r := range roots {
		sslDirs[filepath.Join(r, "ssl")] = true
	}

	var out []backup.SnapshotFile
	seen := map[string]bool{}
	for _, root := range roots {
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // 路径不存在/权限不足：跳过该分支而非整体失败
			}
			if !includeSSL {
				for sd := range sslDirs {
					if p == sd || strings.HasPrefix(p, sd+"/") {
						if info.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
				}
			}
			if seen[p] {
				return nil
			}
			seen[p] = true
			if meta, ok := captureMeta(p, info); ok {
				out = append(out, meta)
			}
			return nil
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "遍历快照路径失败", err)
		}
	}
	return out, nil
}

// captureMeta 采集单文件元数据；无法归档的 socket 返回 ok=false。
func captureMeta(p string, info os.FileInfo) (backup.SnapshotFile, bool) {
	if info.Mode()&os.ModeSocket != 0 {
		return backup.SnapshotFile{}, false
	}
	f := backup.SnapshotFile{
		Path: p,
		Size: info.Size(),
		Mode: int64(info.Mode()),
	}
	if info.Mode().IsRegular() {
		if sum, err := sha256File(p); err == nil {
			f.SHA256 = sum
		}
	}
	f.Owner = ownerString(info)
	return f, true
}

// writeTar 先写元数据条目，再写实际文件条目。
func (e *SnapshotExecutor) writeTar(tarPath string, files []backup.SnapshotFile) error {
	fh, err := os.Create(tarPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "创建快照文件失败", err)
	}
	defer fh.Close()
	gw := gzip.NewWriter(fh)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	metaBytes, err := json.Marshal(files)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "序列化快照元数据失败", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    metaEntryName,
		Mode:    0o600,
		Size:    int64(len(metaBytes)),
		ModTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(metaBytes); err != nil {
		return err
	}

	for _, f := range files {
		if err := e.addFileToTar(tw, f); err != nil {
			return err
		}
	}
	return nil
}

// addFileToTar 把单个文件（含目录/符号链接）加入归档，条目名取相对根（去前导 /）。
func (e *SnapshotExecutor) addFileToTar(tw *tar.Writer, f backup.SnapshotFile) error {
	info, err := os.Lstat(f.Path)
	if err != nil {
		return nil // 文件可能已消失，跳过
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "构造 tar 头失败", err)
	}
	hdr.Name = strings.TrimPrefix(f.Path, "/")
	hdr.Mode = int64(f.Mode) // 用记录的权限位（含特殊位）
	if info.Mode()&os.ModeSymlink != 0 {
		if link, lerr := os.Readlink(f.Path); lerr == nil {
			hdr.Linkname = link
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "写 tar 头失败", err)
	}
	if info.Mode().IsRegular() {
		src, err := os.Open(f.Path)
		if err != nil {
			return nil
		}
		defer src.Close()
		if _, err := io.Copy(tw, src); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "写 tar 内容失败", err)
		}
	}
	return nil
}

// RestoreRequest 是一次恢复请求。
type RestoreRequest struct {
	TarPath string // 快照 tar.gz 绝对路径
	Root    string // 恢复根目录，默认 "/"
}

// Restore 从快照恢复文件到原绝对路径，并还原权限与属主。
// 先全量解包，再按内嵌元数据逐文件 chmod/chown（符号链接不调 chmod/chown，避免改到目标）。
func (e *SnapshotExecutor) Restore(ctx context.Context, req RestoreRequest) error {
	root := req.Root
	if root == "" {
		root = "/"
	}
	fh, err := os.Open(req.TarPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "打开快照失败", err)
	}
	defer fh.Close()
	gr, err := gzip.NewReader(fh)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "解压快照失败", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	var meta []backup.SnapshotFile
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "读取快照流失败", err)
		}
		if hdr.Name == metaEntryName {
			var m []backup.SnapshotFile
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "解析快照元数据失败", err)
			}
			meta = m
			continue // 元数据不落盘
		}
		abs := filepath.Join(root, hdr.Name)
		if !withinRoot(root, abs) {
			continue // 防路径穿越
		}
		if err := extractEntry(tr, hdr, abs); err != nil {
			return err
		}
	}
	// 还原权限与属主
	for i := range meta {
		abs := meta[i].Path
		if !withinRoot(root, abs) {
			continue
		}
		applyMeta(abs, meta[i])
	}
	return nil
}

// extractEntry 把单个 tar 条目落到 abs（目录/符号链接/常规文件分别处理）。
func extractEntry(tr *tar.Reader, hdr *tar.Header, abs string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(abs, os.FileMode(hdr.Mode))
	case tar.TypeSymlink:
		_ = os.MkdirAll(filepath.Dir(abs), 0o700)
		_ = os.Remove(abs)
		return os.Symlink(hdr.Linkname, abs)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "创建父目录失败", err)
		}
		out, err := os.OpenFile(abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "写文件失败", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return apperr.Wrap(apperr.CodeInternal, "写文件内容失败", err)
		}
		return out.Close()
	}
	return nil
}

// applyMeta 还原权限与属主（符号链接跳过，避免 chmod/chown 作用到目标）。
func applyMeta(abs string, f backup.SnapshotFile) {
	mode := os.FileMode(f.Mode)
	if mode&os.ModeSymlink != 0 {
		return
	}
	_ = os.Chmod(abs, mode)
	if uid, gid, ok := resolveOwner(f.Owner); ok {
		_ = os.Chown(abs, uid, gid)
	}
}

// withinRoot 判断 abs 是否落在 root 之内（防路径穿越）。
func withinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// sha256File 计算常规文件 SHA256（十六进制）。
func sha256File(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// ownerString 从文件信息解析属主，优先 "name:name"，失败回退 "uid:gid"。
func ownerString(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "0:0"
	}
	uid, gid := int(st.Uid), int(st.Gid)
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
			return u.Username + ":" + g.Name
		}
		return u.Username + ":" + strconv.Itoa(gid)
	}
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
}

// resolveOwner 把 "name:name" 或 "uid:gid" 解析为 (uid, gid)。
func resolveOwner(s string) (int, int, bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	uid, uerr := lookupID(parts[0], false)
	gid, gerr := lookupID(parts[1], true)
	if uerr != nil || gerr != nil {
		return 0, 0, false
	}
	return uid, gid, true
}

// lookupID 解析用户(u=false)或组(u=true)的 id；接受数字或名称。
func lookupID(s string, isGroup bool) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	if isGroup {
		g, err := user.LookupGroup(s)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(g.Gid)
	}
	u, err := user.Lookup(s)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}
