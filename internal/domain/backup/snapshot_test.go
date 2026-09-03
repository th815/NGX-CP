// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func snapWithTime(t time.Time) ConfigSnapshot {
	return ConfigSnapshot{ID: 1, NodeID: 1, Path: "/x", CreatedAt: t}
}

func TestDefaultSnapshotPolicy(t *testing.T) {
	p := DefaultSnapshotPolicy()
	assert.Equal(t, 90, p.KeepDays)
	assert.Equal(t, 200, p.KeepMaxPerNode)
	assert.False(t, p.IncludeSSL)
}

func TestPathsForRole(t *testing.T) {
	// 普通 RS 只快照 /etc/nginx
	rs := PathsForRole("rs", false)
	assert.Equal(t, []string{"/etc/nginx"}, rs)

	// director 额外快照 /etc/keepalived
	dir := PathsForRole("director", false)
	assert.Contains(t, dir, "/etc/nginx")
	assert.Contains(t, dir, "/etc/keepalived")

	// includeSSL 为真时含 /etc/nginx/ssl
	withSSL := PathsForRole("rs", true)
	assert.Contains(t, withSSL, "/etc/nginx/ssl")
}

func TestFilterExpired(t *testing.T) {
	p := SnapshotPolicy{KeepDays: 90, KeepMaxPerNode: 200}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := snapWithTime(now.AddDate(0, 0, -91)) // 91 天前 → 过期
	fresh := snapWithTime(now.AddDate(0, 0, -10))

	got := p.FilterExpired([]ConfigSnapshot{old, fresh}, now)
	assert.Len(t, got, 1)
	assert.Equal(t, old.ID, got[0].ID)

	// KeepDays<=0 永不过期
	never := SnapshotPolicy{KeepDays: 0}
	assert.Nil(t, never.FilterExpired([]ConfigSnapshot{old}, now))
}

func TestExcessPerNode(t *testing.T) {
	p := SnapshotPolicy{KeepDays: 90, KeepMaxPerNode: 2}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s1 := snapWithTime(base)
	s2 := snapWithTime(base.Add(time.Hour))
	s3 := snapWithTime(base.Add(2 * time.Hour))

	// 3 份超过 2 份 → 超出的 1 份是最旧的 s1
	got := p.ExcessPerNode([]ConfigSnapshot{s3, s1, s2})
	assert.Len(t, got, 1)
	assert.Equal(t, s1.ID, got[0].ID)

	// 未超限
	assert.Nil(t, p.ExcessPerNode([]ConfigSnapshot{s1, s2}))
	// KeepMaxPerNode<=0 → 永不裁剪
	assert.Nil(t, SnapshotPolicy{KeepMaxPerNode: 0}.ExcessPerNode([]ConfigSnapshot{s1}))
}
