<script setup>
// 音色选择器。
//
// 为什么不用 <input list="...">：datalist 的过滤是"按已输入的内容筛候选"。
// 一旦值正好等于某个音色 ID，浏览器就把它当成搜索词——候选只剩它自己，
// 看起来就是"下拉出不来了，得先把内容删掉"。实测就是用户报的这个现象。
//
// 改成原生 <select>：点开永远显示全部候选（浏览器自带首字母跳转与滚动），
// 选完还能再点开换。列表里没有的音色走「手动输入」那一项，保留粘贴能力
// （豆包/复刻音色常常是直接粘 ID）。
import { computed, ref, watch } from 'vue'

const props = defineProps({
  modelValue: { type: String, default: '' },
  voices: { type: Array, default: () => [] },
  // 空值时显示的文字：全局用「平台默认音色」，档案用「跟随全局」
  placeholder: { type: String, default: '平台默认音色' },
  // 是否允许清空（档案里"跟随全局"需要）
  allowEmpty: { type: Boolean, default: true },
})
const emit = defineEmits(['update:modelValue'])

const MANUAL = '__manual__'
const custom = ref(false)

const ids = computed(() => new Set(props.voices.map((v) => v.id)))
// 当前值不在清单里（手填过 / 清单换了平台）就进手动态，否则 select 会显示空白，
// 让人以为音色丢了
const isManualValue = computed(
  () => !!props.modelValue && !ids.value.has(props.modelValue),
)

watch(
  () => [props.modelValue, props.voices],
  () => {
    if (isManualValue.value) custom.value = true
  },
  { immediate: true },
)

const manual = computed(() => custom.value || isManualValue.value)

// 分组：官方 / 复刻 / 自定义清单。空格不在选项里表示"平台默认"。
const groups = computed(() => {
  const official = props.voices.filter((v) => !v.cloned)
  const cloned = props.voices.filter((v) => v.cloned)
  return [
    { label: '官方音色', items: official },
    { label: '我的音色（复刻 / 自定义清单）', items: cloned },
  ].filter((g) => g.items.length)
})

function pick(ev) {
  const v = ev.target.value
  if (v === MANUAL) {
    custom.value = true
    return
  }
  custom.value = false
  emit('update:modelValue', v)
}

function clear() {
  custom.value = false
  emit('update:modelValue', '')
}

// 选中项的展示名：清单里没有就别硬凑，直接显示 ID
const currentName = computed(() => {
  const hit = props.voices.find((v) => v.id === props.modelValue)
  return hit ? hit.name : ''
})
</script>

<template>
  <div class="voice-select">
    <div class="flex items-center gap-8">
      <select v-if="!manual" class="input" :value="modelValue" @change="pick">
        <option v-if="allowEmpty" value="">{{ placeholder }}</option>
        <optgroup v-for="g in groups" :key="g.label" :label="g.label">
          <option v-for="v in g.items" :key="v.id" :value="v.id">
            {{ v.name }} · {{ v.id }}
          </option>
        </optgroup>
        <option :value="MANUAL">手动输入音色 ID…</option>
      </select>

      <template v-else>
        <input
          class="input mono grow"
          :value="modelValue"
          placeholder="粘贴音色 ID，例如 zh_female_vv_uranus_bigtts"
          @input="emit('update:modelValue', $event.target.value)"
        />
        <button v-if="voices.length" class="btn sm ghost" @click="custom = false">
          从清单选
        </button>
      </template>

      <button v-if="modelValue && allowEmpty" class="btn sm ghost" @click="clear">清空</button>
    </div>

    <div v-if="currentName" class="tiny mute mt-6">
      当前：{{ currentName }}（{{ modelValue }}）
    </div>
    <div v-else-if="modelValue" class="tiny mute mt-6 mono">当前：{{ modelValue }}</div>
  </div>
</template>
