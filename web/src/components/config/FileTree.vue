<script setup lang="ts">
import { computed } from 'vue'
import { NEmpty } from 'naive-ui'
import type { ConfigFileItem } from '@/api/configs'

const props = defineProps<{
  files: ConfigFileItem[]
  selectedId: number | null
  driftPaths?: string[]
}>()

const emit = defineEmits<{ select: [id: number] }>()

// 按顶层目录分组（nginx.conf 归入「根」）。
const groups = computed(() => {
  const map = new Map<string, ConfigFileItem[]>()
  for (const f of props.files) {
    const seg = f.path.split('/')
    const dir = seg.length > 1 ? seg.slice(0, -1).join('/') : '（根）'
    if (!map.has(dir)) map.set(dir, [])
    map.get(dir)!.push(f)
  }
  return Array.from(map.entries()).map(([dir, items]) => ({
    dir,
    items: items.slice().sort((a, b) => a.path.localeCompare(b.path))
  }))
})

function isDrifted(path: string) {
  return (props.driftPaths ?? []).includes(path)
}
</script>

<template>
  <div class="tree">
    <n-empty v-if="!files.length" description="该节点暂无配置快照" size="small" />
    <div v-for="g in groups" :key="g.dir" class="group">
      <div class="dir">{{ g.dir }}</div>
      <div
        v-for="f in g.items"
        :key="f.id"
        class="file"
        :class="{ active: f.id === selectedId, drift: isDrifted(f.path) }"
        @click="emit('select', f.id)"
      >
        <span class="name">{{ f.path.split('/').pop() }}</span>
        <span v-if="isDrifted(f.path)" class="dot" title="检测到漂移" />
        <span v-else class="sha">{{ f.current_sha.slice(0, 8) }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.tree {
  height: 100%;
  overflow: auto;
  padding: 6px 0;
  font-size: 13px;
}
.group {
  margin-bottom: 6px;
}
.dir {
  padding: 4px 12px;
  color: #8a8f99;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.4px;
}
.file {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 5px 12px 5px 22px;
  cursor: pointer;
  border-left: 2px solid transparent;
}
.file:hover {
  background: rgba(24, 160, 88, 0.08);
}
.file.active {
  background: rgba(24, 160, 88, 0.16);
  border-left-color: #18a058;
}
.file.drift {
  background: rgba(224, 108, 117, 0.1);
}
.name {
  font-family: monospace;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sha {
  color: #b0b6c0;
  font-family: monospace;
  font-size: 11px;
  flex: 0 0 auto;
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #e06c75;
  flex: 0 0 auto;
}
</style>
