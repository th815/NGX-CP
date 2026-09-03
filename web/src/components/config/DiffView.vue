<script setup lang="ts">
import type { DiffResult } from '@/api/configs'

defineProps<{ diff: DiffResult | null }>()

const cls = (t: string) => (t === 'add' ? 'add' : t === 'del' ? 'del' : 'ctx')
</script>

<template>
  <div class="diff" v-if="diff">
    <div class="stats">
      <span class="badge add">+{{ diff.stats.added }}</span>
      <span class="badge del">-{{ diff.stats.deleted }}</span>
      <span class="badge chg">~{{ diff.stats.changed }}</span>
    </div>
    <div v-if="!diff.hunks.length" class="same">两版内容一致</div>
    <div v-for="(h, i) in diff.hunks" :key="i" class="hunk">
      <div class="hunk-head">
        @@ -{{ h.old_start }}{{ h.old_lines ? ',' + h.old_lines : '' }}
        +{{ h.new_start }}{{ h.new_lines ? ',' + h.new_lines : '' }} @@
      </div>
      <div
        v-for="(l, j) in h.lines"
        :key="j"
        class="line"
        :class="cls(l.type)"
      >
        <span class="no">{{ l.type === 'add' ? '' : l.old_no }}</span>
        <span class="no">{{ l.type === 'del' ? '' : l.new_no }}</span>
        <span class="sign">{{ l.type === 'add' ? '+' : l.type === 'del' ? '-' : ' ' }}</span>
        <span class="txt">{{ l.content }}</span>
      </div>
    </div>
  </div>
  <div v-else class="same">暂无差异数据</div>
</template>

<style scoped>
.diff {
  font-family: 'SFMono-Regular', Consolas, Menlo, monospace;
  font-size: 12.5px;
  line-height: 19px;
  background: #1e1e1e;
  color: #d4d4d4;
  border-radius: 6px;
  padding: 10px 12px;
  overflow: auto;
  max-height: 420px;
}
.stats {
  margin-bottom: 8px;
}
.badge {
  display: inline-block;
  padding: 1px 7px;
  border-radius: 10px;
  font-size: 11px;
  margin-right: 6px;
}
.badge.add {
  background: rgba(78, 201, 127, 0.18);
  color: #4ec97f;
}
.badge.del {
  background: rgba(224, 108, 117, 0.18);
  color: #e06c75;
}
.badge.chg {
  background: rgba(229, 192, 123, 0.18);
  color: #e5c07b;
}
.hunk-head {
  color: #61afef;
  background: rgba(97, 175, 239, 0.08);
  padding: 2px 6px;
  margin: 6px 0 2px;
  border-radius: 3px;
}
.line {
  display: flex;
  white-space: pre;
}
.line .no {
  width: 34px;
  text-align: right;
  color: #6b6b6b;
  user-select: none;
  flex: 0 0 34px;
}
.line .sign {
  width: 16px;
  flex: 0 0 16px;
  user-select: none;
}
.line.add {
  background: rgba(78, 201, 127, 0.16);
}
.line.del {
  background: rgba(224, 108, 117, 0.16);
}
.line.add .txt {
  color: #98c379;
}
.line.del .txt {
  color: #e06c75;
}
.same {
  color: #8a8f99;
  font-size: 13px;
  padding: 8px 2px;
}
</style>
