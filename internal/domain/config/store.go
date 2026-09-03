// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package config 实现配置中心的核心领域逻辑：配置树同步、内容寻址存储、
// 版本链、Diff、校验规则、漂移检测、模板渲染。本文件落地 T021 的内容寻址存储。
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/configblob"
	"github.com/th/ngxcp/ent/configfile"
	"github.com/th/ngxcp/ent/configrevision"
)

// Source 标识一个配置版本的来源（T021）。
type Source string

const (
	SourceSync          Source = "sync"           // Agent 同步上报（nginx -T）
	SourceManualEdit    Source = "manual_edit"    // 控制台手动编辑
	SourceCertRenew     Source = "cert_renew"     // 证书续期触发的配置变更
	SourceSecurityBlock Source = "security_block" // 安全封禁片段下发
	SourceRollback      Source = "rollback"       // 回滚产生的版本
)

// RevisionOpts 创建版本时的可选参数。
type RevisionOpts struct {
	Source        Source
	Author        string
	Message       string
	ChangeOrderID int // 0 表示无关联变更单
}

// FileView 是配置文件的视图（含当前版本摘要），供 API 返回。
type FileView struct {
	ID             int
	NodeID         int
	Path           string
	Format         string
	CurrentRevID   int
	CurrentSHA     string
	CurrentSize    int
	CurrentContent string // 仅在显式获取时填充
	Source         string
	Author         string
	UpdatedAt      time.Time
	CreatedAt      time.Time
}

// RevisionView 是单个配置版本的视图。
type RevisionView struct {
	ID            int
	NodeID        int
	Path          string
	SHA256        string
	Source        string
	Author        string
	Message       string
	ParentID      int
	ChangeOrderID int
	CreatedAt     time.Time
}

// ConfigStore 配置版本化存储：内容寻址 + 版本链 + 去重。
// 所有方法围绕 ent 客户端实现，事务内部保证版本链连续（避免并发断链）。
type ConfigStore struct {
	client *ent.Client
}

// New 构造配置存储。
func New(client *ent.Client) *ConfigStore {
	return &ConfigStore{client: client}
}

// sha256Hex 计算内容哈希（与 Agent 侧 ParseConfigTree 保持一致，便于比对）。
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// PutBlob 内容寻址写入：相同内容（同 sha256）只存一份。返回内容哈希。
func (s *ConfigStore) PutBlob(ctx context.Context, content []byte) (string, error) {
	sha := sha256Hex(content)
	if err := s.putBlob(ctx, sha, content); err != nil {
		return "", err
	}
	return sha, nil
}

// putBlob 幂等写入 blob：已存在则跳过。
func (s *ConfigStore) putBlob(ctx context.Context, sha string, content []byte) error {
	exist, err := s.client.ConfigBlob.Query().Where(configblob.Sha256(sha)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("query blob %s: %w", sha, err)
	}
	if exist {
		return nil
	}
	if _, err := s.client.ConfigBlob.Create().
		SetSha256(sha).
		SetSize(len(content)).
		SetContent(string(content)).
		Save(ctx); err != nil {
		return fmt.Errorf("create blob %s: %w", sha, err)
	}
	return nil
}

// GetBlob 按哈希取回配置内容。
func (s *ConfigStore) GetBlob(ctx context.Context, sha string) ([]byte, error) {
	b, err := s.client.ConfigBlob.Query().Where(configblob.Sha256(sha)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("blob %s 不存在", sha)
		}
		return nil, fmt.Errorf("get blob %s: %w", sha, err)
	}
	return []byte(b.Content), nil
}

// SyncFromAgent 把 Agent 上报的配置树同步进版本化存储。
//   - 相同内容只存一份（blob 去重）；
//   - 内容未变不产生新版本（changed 不计数）；
//   - 内容变化产生新版本，parent 指向上一版（事务内保证链连续）；
//   - 返回本次"内容发生变化"的文件数。
func (s *ConfigStore) SyncFromAgent(ctx context.Context, nodeID int, files []*agentv1.ConfigFile) (int, error) {
	changed := 0
	for i := range files {
		f := files[i]
		if f == nil {
			continue
		}
		path := f.GetPath()
		if path == "" {
			continue
		}
		content := []byte(f.GetContent())
		sha := f.GetSha256()
		if sha == "" {
			sha = sha256Hex(content)
		}
		if err := s.putBlob(ctx, sha, content); err != nil {
			return changed, fmt.Errorf("put blob %s: %w", path, err)
		}

		cf, err := s.upsertConfigFile(ctx, nodeID, path)
		if err != nil {
			return changed, fmt.Errorf("upsert config file %s: %w", path, err)
		}

		// 当前版本 blob 是否与上报一致？一致则跳过（不产生新版本）。
		cur, err := s.currentRevision(ctx, cf)
		if err != nil {
			return changed, fmt.Errorf("query current revision %s: %w", path, err)
		}
		if cur != nil {
			curBlob, berr := cur.QueryBlob().Only(ctx)
			if berr != nil && !ent.IsNotFound(berr) {
				return changed, fmt.Errorf("query blob of current revision %s: %w", path, berr)
			}
			if curBlob != nil && curBlob.Sha256 == sha {
				continue // 内容未变
			}
		}

		var parentID int
		if cur != nil {
			parentID = cur.ID
		}
		if err := s.createRevision(ctx, cf.ID, nodeID, path, sha, parentID, RevisionOpts{
			Source: SourceSync,
			Author: "agent",
		}); err != nil {
			return changed, fmt.Errorf("create revision %s: %w", path, err)
		}
		changed++
	}
	return changed, nil
}

// upsertConfigFile 按 (node_id, path) 查找或创建逻辑配置文件。
func (s *ConfigStore) upsertConfigFile(ctx context.Context, nodeID int, path string) (*ent.ConfigFile, error) {
	cf, err := s.client.ConfigFile.Query().
		Where(configfile.NodeID(nodeID), configfile.Path(path)).
		Only(ctx)
	if err == nil {
		return cf, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	return s.client.ConfigFile.Create().
		SetNodeID(nodeID).
		SetPath(path).
		Save(ctx)
}

// createRevision 在事务内创建新版本并将 ConfigFile.current_revision 指向它，
// 同时把 parent 指向上一版，保证版本链连续。
func (s *ConfigStore) createRevision(ctx context.Context, fileID, nodeID int, path, sha string, parentID int, opts RevisionOpts) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("open tx: %w", err)
	}
	blob, err := tx.ConfigBlob.Query().Where(configblob.Sha256(sha)).Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("query blob %s: %w", sha, err)
	}
	rc := tx.ConfigRevision.Create().
		SetNodeID(nodeID).
		SetPath(path).
		SetSource(configrevision.Source(opts.Source)).
		SetAuthor(opts.Author).
		SetMessage(opts.Message).
		SetBlob(blob)
	if opts.ChangeOrderID > 0 {
		rc = rc.SetChangeOrderID(opts.ChangeOrderID)
	}
	if parentID > 0 {
		rc = rc.SetParentID(parentID)
	}
	rev, err := rc.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("save revision: %w", err)
	}
	// 用字段级 FK 标记当前生效版本（current_revision 边已移除，改为 current_revision_id 字段）
	if _, err := tx.ConfigFile.UpdateOneID(fileID).SetCurrentRevisionID(rev.ID).Save(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set current revision: %w", err)
	}
	return tx.Commit()
}

// CreateRevision 主动创建一版配置（手动编辑 / 证书续期 / 回滚等来源），内容寻址写入。
func (s *ConfigStore) CreateRevision(ctx context.Context, fileID int, content []byte, opts RevisionOpts) (*RevisionView, error) {
	cf, err := s.client.ConfigFile.Get(ctx, fileID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("config file %d 不存在", fileID)
		}
		return nil, fmt.Errorf("get config file: %w", err)
	}
	sha := sha256Hex(content)
	if err := s.putBlob(ctx, sha, content); err != nil {
		return nil, err
	}
	cur, err := s.currentRevision(ctx, cf)
	if err != nil {
		return nil, fmt.Errorf("query current revision: %w", err)
	}
	var parentID int
	if cur != nil {
		parentID = cur.ID
	}
	if err := s.createRevision(ctx, cf.ID, cf.NodeID, cf.Path, sha, parentID, opts); err != nil {
		return nil, err
	}
	return s.GetRevision(ctx, sha) // 取回刚写入版本（同 sha 唯一）
}

// GetRevision 按 (path, sha) 取回版本视图（用于 CreateRevision 回查）。
func (s *ConfigStore) GetRevision(ctx context.Context, sha string) (*RevisionView, error) {
	rev, err := s.client.ConfigRevision.Query().
		Where(configrevision.HasBlobWith(configblob.Sha256(sha))).
		Order(ent.Desc("created_at")).
		First(ctx)
	if err != nil {
		return nil, fmt.Errorf("query revision by blob sha: %w", err)
	}
	return revisionToView(ctx, rev), nil
}

// ListRevisions 列出某配置文件的版本链（按时间倒序），limit<=0 表示不限制。
// 版本经 ConfigRevision 的 node_id+path 字段与文件关联（config_file 边已移除），
// 故先取文件得到 node_id/path，再按字段过滤。
func (s *ConfigStore) ListRevisions(ctx context.Context, fileID, limit int) ([]*RevisionView, error) {
	cf, err := s.client.ConfigFile.Get(ctx, fileID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("config file %d 不存在", fileID)
		}
		return nil, fmt.Errorf("get config file: %w", err)
	}
	q := s.client.ConfigRevision.Query().
		Where(configrevision.NodeID(cf.NodeID), configrevision.Path(cf.Path)).
		Order(ent.Desc("created_at"))
	if limit > 0 {
		q = q.Limit(limit)
	}
	revs, err := q.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	out := make([]*RevisionView, 0, len(revs))
	for _, r := range revs {
		out = append(out, revisionToView(ctx, r))
	}
	return out, nil
}

// ListFiles 列出某节点的所有配置文件（含当前版本摘要）。
func (s *ConfigStore) ListFiles(ctx context.Context, nodeID int) ([]*FileView, error) {
	files, err := s.client.ConfigFile.Query().
		Where(configfile.NodeID(nodeID)).
		Order(ent.Asc("path")).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list config files: %w", err)
	}
	out := make([]*FileView, 0, len(files))
	for _, f := range files {
		v := &FileView{
			ID:        f.ID,
			NodeID:    f.NodeID,
			Path:      f.Path,
			Format:    string(f.Format),
			CreatedAt: f.CreatedAt,
			UpdatedAt: f.UpdatedAt,
		}
		cur, err := s.currentRevision(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("query current revision: %w", err)
		}
		if cur != nil {
			v.CurrentRevID = cur.ID
			v.UpdatedAt = cur.CreatedAt
			v.Source = string(cur.Source)
			v.Author = cur.Author
			b, berr := cur.QueryBlob().Only(ctx)
			if berr != nil && !ent.IsNotFound(berr) {
				return nil, fmt.Errorf("query blob: %w", berr)
			}
			if b != nil {
				v.CurrentSHA = b.Sha256
				v.CurrentSize = b.Size
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// GetFile 取单个配置文件视图。
func (s *ConfigStore) GetFile(ctx context.Context, fileID int) (*FileView, error) {
	f, err := s.client.ConfigFile.Get(ctx, fileID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("config file %d 不存在", fileID)
		}
		return nil, fmt.Errorf("get config file: %w", err)
	}
	return s.fileToView(ctx, f)
}

// GetCurrentContent 取某配置文件当前版本的内容。
func (s *ConfigStore) GetCurrentContent(ctx context.Context, fileID int) ([]byte, error) {
	f, err := s.client.ConfigFile.Get(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("get config file: %w", err)
	}
	cur, err := s.currentRevision(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("query current revision: %w", err)
	}
	if cur == nil {
		return nil, fmt.Errorf("config file %d 无当前版本", fileID)
	}
	b, err := cur.QueryBlob().Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("query blob: %w", err)
	}
	return []byte(b.Content), nil
}

// currentRevision 按 ConfigFile.current_revision_id 取回当前生效版本（无则用 nil）。
func (s *ConfigStore) currentRevision(ctx context.Context, cf *ent.ConfigFile) (*ent.ConfigRevision, error) {
	if cf.CurrentRevisionID == 0 {
		return nil, nil
	}
	rev, err := s.client.ConfigRevision.Get(ctx, cf.CurrentRevisionID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rev, nil
}

// ExpectedRevision 返回平台「期望」版本：漂移检测的对比基准。
//
// 设计要点：SyncFromAgent 每次都会把 current_revision_id 覆盖为最新一次 sync（节点实际上报），
// 若直接拿 current_revision 当「期望」，平台会悄悄"采纳"手工改动、漂移永远检不出。
// 因此基准优先取**最近一次平台主动产生的版本**（manual_edit / cert_renew / security_block /
// rollback）；节点从未被平台主动改过时才回退到**最早一次 sync**（首次纳管基线）。
// 这样手工在节点上 vi 改配置 → 新的 sync 版本与基线/平台版本 SHA 不符 → 持久化漂移告警。
func (s *ConfigStore) ExpectedRevision(ctx context.Context, nodeID int, path string) (*ent.ConfigRevision, error) {
	managed, err := s.client.ConfigRevision.Query().
		Where(
			configrevision.NodeID(nodeID),
			configrevision.Path(path),
			configrevision.SourceIn(
				configrevision.SourceManualEdit,
				configrevision.SourceCertRenew,
				configrevision.SourceSecurityBlock,
				configrevision.SourceRollback,
			),
		).
		Order(ent.Desc("created_at")).
		First(ctx)
	if err == nil {
		return managed, nil
	}
	if !ent.IsNotFound(err) {
		return nil, fmt.Errorf("query managed revision %s: %w", path, err)
	}
	// 无平台主动版本 → 回退到最早一次 sync 基线。
	baseline, berr := s.client.ConfigRevision.Query().
		Where(configrevision.NodeID(nodeID), configrevision.Path(path), configrevision.SourceEQ(configrevision.SourceSync)).
		Order(ent.Asc("created_at")).
		First(ctx)
	if berr != nil {
		if ent.IsNotFound(berr) {
			return nil, nil // 该路径没有任何版本
		}
		return nil, fmt.Errorf("query baseline revision %s: %w", path, berr)
	}
	return baseline, nil
}

// fileToView 填充单个配置文件的完整视图（含当前版本内容与摘要）。
func (s *ConfigStore) fileToView(ctx context.Context, f *ent.ConfigFile) (*FileView, error) {
	v := &FileView{
		ID:        f.ID,
		NodeID:    f.NodeID,
		Path:      f.Path,
		Format:    string(f.Format),
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
	cur, err := s.currentRevision(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("query current revision: %w", err)
	}
	if cur != nil {
		v.CurrentRevID = cur.ID
		v.UpdatedAt = cur.CreatedAt
		v.Source = string(cur.Source)
		v.Author = cur.Author
		b, berr := cur.QueryBlob().Only(ctx)
		if berr != nil && !ent.IsNotFound(berr) {
			return nil, fmt.Errorf("query blob: %w", berr)
		}
		if b != nil {
			v.CurrentSHA = b.Sha256
			v.CurrentSize = b.Size
			v.CurrentContent = b.Content
		}
	}
	return v, nil
}

func revisionToView(ctx context.Context, r *ent.ConfigRevision) *RevisionView {
	v := &RevisionView{
		ID:            r.ID,
		NodeID:        r.NodeID,
		Path:          r.Path,
		Source:        string(r.Source),
		Author:        r.Author,
		Message:       r.Message,
		ChangeOrderID: r.ChangeOrderID,
		CreatedAt:     r.CreatedAt,
	}
	if b, err := r.QueryBlob().Only(ctx); err == nil && b != nil {
		v.SHA256 = b.Sha256
	}
	if p, err := r.QueryParent().Only(ctx); err == nil && p != nil {
		v.ParentID = p.ID
	}
	return v
}
