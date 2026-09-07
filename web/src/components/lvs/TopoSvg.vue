<script setup lang="ts">
import { computed } from 'vue'
import type { Topology, DirectorNode, RealServerNode } from '@/api/lvs'

const props = defineProps<{ topo: Topology }>()

const W = 680
const H = 420

const vip = computed(() => props.topo.vip)
const directors = computed(() => {
  const list = [...props.topo.directors]
  // 主在前，备在后
  list.sort((a, b) => (a.state === 'MASTER' ? -1 : 1) - (b.state === 'MASTER' ? -1 : 1))
  return list
})
const rs = computed(() => [...props.topo.rs])

const vipBox = { x: W / 2 - 90, y: 28, w: 180, h: 52 }

function dirPos(i: number): { x: number; y: number } {
  const n = Math.max(directors.value.length, 1)
  const x = n === 1 ? W / 2 : (W / (n + 1)) * (i + 1)
  return { x: x - 95, y: 160 }
}
function rsPos(i: number): { x: number; y: number } {
  const n = Math.max(rs.value.length, 1)
  const x = n === 1 ? W / 2 : (W / (n + 1)) * (i + 1)
  return { x: x - 80, y: 332 }
}

const dirNodes = computed(() =>
  directors.value.map((d, i) => ({ d, box: dirPos(i), color: d.holding_vip ? '#16a34a' : '#9ca3af' }))
)
const rsNodes = computed(() =>
  rs.value.map((r, i) => ({ r, box: rsPos(i), color: rsColor(r) }))
)

function rsColor(r: RealServerNode): string {
  if (!r.enabled) return '#9ca3af'
  if (r.state === 'down') return '#dc2626'
  if (r.state === 'draining' || r.state === 'down') return '#f59e0b'
  return '#16a34a'
}

function center(b: { x: number; y: number }, w: number, h: number) {
  return { x: b.x + w / 2, y: b.y + h / 2 }
}
const vipC = computed(() => center(vipBox, vipBox.w, vipBox.h))

function stateLabel(d: DirectorNode): string {
  return d.state === 'MASTER' ? '主 (MASTER)' : '备 (BACKUP)'
}
function rsLabel(r: RealServerNode): string {
  const base = r.address || r.rip
  const s = !r.enabled ? '禁用' : r.state === 'down' ? 'DOWN' : r.state === 'draining' ? '排空' : '活跃'
  return `${base} · ${s} · w=${r.weight}`
}
</script>

<template>
  <svg :viewBox="`0 0 ${W} ${H}`" class="topo" xmlns="http://www.w3.org/2000/svg">
    <!-- edges: VIP -> Directors -->
    <line
      v-for="(n, i) in dirNodes"
      :key="'edge-vd-' + i"
      :x1="vipC.x"
      :y1="vipBox.y + vipBox.h"
      :x2="center(n.box, 190, 64).x"
      :y2="n.box.y"
      stroke="#94a3b8"
      stroke-width="2"
    />
    <!-- edges: VIP -> RS (DR: 所有 RS 由 VIP 直连) -->
    <line
      v-for="(n, i) in rsNodes"
      :key="'edge-vr-' + i"
      :x1="vipC.x"
      :y1="vipBox.y + vipBox.h"
      :x2="center(n.box, 160, 56).x"
      :y2="n.box.y"
      :stroke="n.color"
      stroke-width="2"
      stroke-dasharray="4 3"
    />

    <!-- VIP -->
    <g>
      <rect :x="vipBox.x" :y="vipBox.y" :width="vipBox.w" :height="vipBox.h" rx="8" fill="#1e3a8a" />
      <text :x="vipC.x" :y="vipBox.y + 22" text-anchor="middle" fill="#fff" font-size="13" font-weight="600">
        VIP
      </text>
      <text :x="vipC.x" :y="vipBox.y + 40" text-anchor="middle" fill="#cbd5e1" font-size="12">
        {{ vip || '—' }}
      </text>
    </g>

    <!-- Directors -->
    <g v-for="(n, i) in dirNodes" :key="'dir-' + i">
      <rect :x="n.box.x" :y="n.box.y" width="190" height="64" rx="8" :fill="n.color" opacity="0.95" />
      <text :x="n.box.x + 95" :y="n.box.y + 22" text-anchor="middle" fill="#fff" font-size="13" font-weight="600">
        {{ stateLabel(n.d) }}
      </text>
      <text :x="n.box.x + 95" :y="n.box.y + 42" text-anchor="middle" fill="#f1f5f9" font-size="11">
        {{ n.d.address }} · prio {{ n.d.priority }} · vrid {{ n.d.vrid }}
      </text>
    </g>

    <!-- Real Servers -->
    <g v-for="(n, i) in rsNodes" :key="'rs-' + i">
      <rect :x="n.box.x" :y="n.box.y" width="160" height="56" rx="8" :fill="n.color" />
      <text :x="n.box.x + 80" :y="n.box.y + 24" text-anchor="middle" fill="#fff" font-size="11">
        {{ rsLabel(n.r) }}
      </text>
      <text :x="n.box.x + 80" :y="n.box.y + 42" text-anchor="middle" fill="#f1f5f9" font-size="10">
        {{ n.r.vs_key }}
      </text>
    </g>
  </svg>
</template>

<style scoped>
.topo {
  width: 100%;
  height: auto;
  background: #f8fafc;
  border-radius: 10px;
}
</style>
