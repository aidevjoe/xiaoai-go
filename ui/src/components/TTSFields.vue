<script setup>
// 按服务端下发的 Field 声明渲染表单。
//
// 存在的意义：接一家新 TTS 平台时，服务端只要在自己那个文件里声明
// {key,label,kind,default,options,help}，这里就自动出现对应的输入框，
// 前端一行都不用改。
//
// 用法：
//   <TTSFields :fields="p.fields" v-model="cfg" />
// cfg 是 map[string]string（即 tts_providers[provider]），空串表示"用默认值"。
import { computed, ref } from 'vue'

const props = defineProps({
  fields: { type: Array, default: () => [] },
  modelValue: { type: Object, default: () => ({}) },
  // 是否把 advanced 的项折起来（配置面板里通常要折）
  collapsible: { type: Boolean, default: true },
})
const emit = defineEmits(['update:modelValue'])

const showAdvanced = ref(false)

const basic = computed(() => props.fields.filter((f) => !f.advanced))
const advanced = computed(() => props.fields.filter((f) => f.advanced))
// 一张表只遍历一次：折叠只影响"包不包含 advanced"，不用写两套渲染分支
const visible = computed(() =>
  props.collapsible && !showAdvanced.value ? basic.value : props.fields,
)

function valueOf(f) {
  const v = props.modelValue?.[f.key]
  return v === undefined || v === null ? f.default || '' : v
}

function set(f, v) {
  const next = { ...(props.modelValue || {}) }
  const s = v === null || v === undefined ? '' : String(v)
  // 空值不写进配置：让后端用默认值，而不是把一个空串当成"用户明确要空"
  if (s === '') delete next[f.key]
  else next[f.key] = s
  emit('update:modelValue', next)
}

const asBool = (v) => v === 'true' || v === '1' || v === 'yes' || v === 'on'

function inputType(f) {
  if (f.kind === 'password') return 'password'
  if (f.kind === 'number') return 'number'
  return 'text'
}
</script>

<template>
  <div class="tts-fields">
    <div
      v-for="f in visible"
      :key="f.key"
      class="field"
      :class="f.kind === 'textarea' ? 'wide' : ''"
    >
      <div class="field-label">
        <span>{{ f.label || f.key }}</span>
        <span v-if="f.required" class="tag bad">必填</span>
        <span v-if="f.advanced" class="tag">高级</span>
      </div>

      <select
        v-if="f.kind === 'select'"
        class="input"
        :value="valueOf(f)"
        @change="set(f, $event.target.value)"
      >
        <option value="">（默认）</option>
        <option v-for="o in f.options || []" :key="o.value" :value="o.value">{{ o.label }}</option>
      </select>

      <label v-else-if="f.kind === 'bool'" class="switch">
        <input
          type="checkbox"
          :checked="asBool(valueOf(f))"
          @change="set(f, $event.target.checked ? 'true' : 'false')"
        />
        <span class="track"></span>
      </label>

      <textarea
        v-else-if="f.kind === 'textarea'"
        class="textarea"
        rows="2"
        :value="valueOf(f)"
        :placeholder="f.default || ''"
        @input="set(f, $event.target.value)"
      ></textarea>

      <input
        v-else
        class="input mono"
        :type="inputType(f)"
        :value="valueOf(f)"
        :placeholder="f.default || ''"
        @input="set(f, $event.target.value)"
      />

      <div v-if="f.help" class="field-hint">{{ f.help }}</div>
    </div>

    <button
      v-if="collapsible && advanced.length"
      class="btn sm ghost"
      type="button"
      @click="showAdvanced = !showAdvanced"
    >
      {{ showAdvanced ? '收起高级项' : `高级项（${advanced.length}）` }}
    </button>
  </div>
</template>
