<script setup lang="ts">
import { computed, ref, watch, nextTick, onMounted } from 'vue'
import hljs from 'highlight.js/lib/core'
import nginx from 'highlight.js/lib/languages/nginx'
import 'highlight.js/styles/atom-one-dark.css'

// 注册 nginx 语言（按需引入，避免全量打包）。
hljs.registerLanguage('nginx', nginx)

const props = withDefaults(
  defineProps<{
    modelValue: string
    readOnly?: boolean
    errorLines?: number[]
    placeholder?: string
  }>(),
  { readOnly: false, errorLines: () => [], placeholder: '' }
)

const emit = defineEmits<{
  'update:modelValue': [value: string]
  save: []
}>()

const taRef = ref<HTMLTextAreaElement | null>(null)
const preRef = ref<HTMLPreElement | null>(null)
const gutterRef = ref<HTMLDivElement | null>(null)
const highlighted = ref('')
const lineCount = ref(1)

// 高亮：尾随换行时补一个空格，避免最后一行错位。
function renderHighlight() {
  const v = props.modelValue
  const src = v.endsWith('\n') ? v + ' ' : v
  highlighted.value = hljs.highlight(src, { language: 'nginx' }).value
  lineCount.value = v.length === 0 ? 1 : v.split('\n').length
}

// 同步滚动：textarea 滚动时同步 pre 与行号 gutter。
function syncScroll() {
  const ta = taRef.value
  if (!ta) return
  if (preRef.value) preRef.value.scrollTop = ta.scrollTop
  if (preRef.value) preRef.value.scrollLeft = ta.scrollLeft
  if (gutterRef.value) gutterRef.value.scrollTop = ta.scrollTop
}

function onInput(e: Event) {
  emit('update:modelValue', (e.target as HTMLTextAreaElement).value)
}

function onKeydown(e: KeyboardEvent) {
  // Ctrl/Cmd + S → 保存（阻止浏览器保存网页）。
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault()
    emit('save')
    return
  }
  // Tab → 插入两个空格而非切换焦点。
  if (e.key === 'Tab' && !props.readOnly) {
    e.preventDefault()
    const ta = taRef.value
    if (!ta) return
    const start = ta.selectionStart
    const end = ta.selectionEnd
    const v = props.modelValue
    const next = v.slice(0, start) + '  ' + v.slice(end)
    emit('update:modelValue', next)
    nextTick(() => {
      ta.selectionStart = ta.selectionEnd = start + 2
    })
  }
}

const errorSet = computed(() => new Set(props.errorLines))

onMounted(renderHighlight)
watch(() => props.modelValue, renderHighlight)
</script>

<template>
  <div class="editor" :class="{ readonly: readOnly }">
    <div ref="gutterRef" class="gutter">
      <div
        v-for="n in lineCount"
        :key="n"
        class="ln"
        :class="{ err: errorSet.has(n) }"
      >
        {{ n }}
      </div>
    </div>

    <div class="surface">
      <pre
        ref="preRef"
        aria-hidden="true"
        class="hl"
        v-html="highlighted"
      /><textarea
        ref="taRef"
        class="ta"
        :value="modelValue"
        :readonly="readOnly"
        :placeholder="placeholder"
        spellcheck="false"
        autocomplete="off"
        autocapitalize="off"
        @input="onInput"
        @scroll="syncScroll"
        @keydown="onKeydown"
      />
    </div>
  </div>
</template>

<style scoped>
.editor {
  display: flex;
  height: 100%;
  min-height: 0;
  background: #282c34;
  border-radius: 6px;
  overflow: hidden;
  font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
  font-size: 13px;
  line-height: 20px;
}
.gutter {
  flex: 0 0 48px;
  padding: 12px 0;
  text-align: right;
  color: #5c6370;
  background: #21252b;
  user-select: none;
  overflow: hidden;
  border-right: 1px solid #181a1f;
}
.ln {
  height: 20px;
  padding-right: 10px;
}
.ln.err {
  color: #e06c75;
  font-weight: 700;
}
.surface {
  position: relative;
  flex: 1 1 auto;
  min-width: 0;
}
.hl,
.ta {
  margin: 0;
  padding: 12px 14px;
  font: inherit;
  line-height: 20px;
  white-space: pre;
  word-wrap: normal;
  tab-size: 2;
  border: 0;
}
.hl {
  position: absolute;
  inset: 0;
  overflow: auto;
  pointer-events: none;
  color: #abb2bf;
}
.ta {
  position: absolute;
  inset: 0;
  overflow: auto;
  resize: none;
  background: transparent;
  color: transparent;
  caret-color: #fff;
  outline: none;
}
.ta::placeholder {
  color: #5c6370;
}
.ta::selection {
  background: rgba(86, 156, 214, 0.35);
}
.editor.readonly .ta {
  cursor: default;
}
</style>
