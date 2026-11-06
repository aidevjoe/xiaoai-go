<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import Ico from '../components/Ico.vue'
import Field from '../components/Field.vue'
import VoiceSelect from '../components/VoiceSelect.vue'
import { state, toast, clone, refreshLive } from '../store'
import { api } from '../api'
import { fmtAgo } from '../utils'

const editing = ref(null) // 抽屉里的局部副本：编辑期间不碰 state.draft
const editingIdx = ref(-1)
const isNew = ref(false) // true = 还没写进 draft 的新档案
// 打开抽屉那一刻的快照（已补齐默认值）。用它判断"用户到底改没改"，
// 别跟 state.draft 里的原始对象比：open() 会补上 name_in_prompt、voice.backend
// 这类缺省字段，直接比会误判为"有未保存改动"。
const baseline = ref(null)
const wakeInput = ref('')
const exitInput = ref('')
const extraText = ref('')
const extraErr = ref('')

// 「扩展参数」的 JSON 框和「系统提示词」都要跟着内容长高。
// 固定行高时，稍长的文本会在框里被切成半行；加上 macOS 的滚动条平时不显示，
// 看起来就像"内容被截断了、还滑不动"。让它自动撑开，超过上限才在框内滚。
const extraBox = ref(null)
const promptBox = ref(null)
const EXTRA_MAX_H = 260
const PROMPT_MAX_H = 320
function fitBox(el, max) {
  if (!el) return
  el.style.height = 'auto'
  // +2：scrollHeight 不含上下各 1px 的边框，直接赋值会让 content 比可视区高 2px，
  // 明明放得下也会冒出一条滚动条。
  el.style.height = Math.min(el.scrollHeight + 2, max) + 'px'
}
function fitBoxes() {
  fitBox(extraBox.value, EXTRA_MAX_H)
  fitBox(promptBox.value, PROMPT_MAX_H)
}

const profiles = computed(() => state.draft?.profiles || [])
const syncing = ref(false)

function devTag(d) {
  if (!d.online) return { kind: '', text: '不在线' }
  if (d.rev === '(未同步)') return { kind: 'warn', text: '未同步' }
  if (d.err) return { kind: 'bad', text: '上次失败' }
  if (d.ok && d.rev === state.kws.rev) return { kind: 'ok', text: d.skip ? '已是最新' : '已同步' }
  if (d.ok) return { kind: 'warn', text: '版本落后' }
  return { kind: 'warn', text: '待同步' }
}

async function runSync(device) {
  syncing.value = true
  try {
    const r = await api.kwsSync(device)
    const list = Object.values(r.devices || {})
    const bad = list.filter((x) => x.err)
    if (!list.length) toast(r.note || '没有在线设备', 'warn')
    else if (bad.length) toast(`${bad.length} 台设备同步失败：${bad[0].err}`, 'bad', 6000)
    else toast(`已同步到 ${list.length} 台设备`, 'ok')
    await refreshLive()
  } catch (e) {
    toast(e.message, 'bad', 5000)
  } finally {
    syncing.value = false
  }
}

const syncAll = () => runSync('')
const syncOne = (id) => runSync(id)
const toolNames = computed(() => state.meta.tool_names || [])

const toolLabels = {
  ha_search_entities: '搜索设备',
  ha_get_state: '查询状态',
  ha_call_service: '控制设备',
  none: '不使用任何工具',
}

function open(p, idx) {
  editing.value = clone(p)
  editingIdx.value = idx
  extraText.value = p.llm?.extra && Object.keys(p.llm.extra).length
    ? JSON.stringify(p.llm.extra, null, 2)
    : ''
  extraErr.value = ''
  wakeInput.value = ''
  exitInput.value = ''
  if (!editing.value.prefetch) {
    editing.value.prefetch = { time: true, weather: true, devices: true }
  }
  if (!editing.value.tools) editing.value.tools = []
  if (!editing.value.voice) editing.value.voice = {}
  // select 的「跟随全局」是空串；undefined 会让浏览器显示成空白框
  if (editing.value.voice.backend === undefined) editing.value.voice.backend = ''
  // 老配置没有这个字段时默认开启：命名为「管家」就要能回答「我是管家」
  if (editing.value.name_in_prompt === undefined || editing.value.name_in_prompt === null) {
    editing.value.name_in_prompt = true
  }
  isNew.value = false
  baseline.value = JSON.stringify(editing.value)
  loadProfileVoices()
  // 抽屉刚渲染时量不到真实高度，等一帧再撑开
  nextTick(fitBoxes)
}

// ---- 发声覆盖（不同助手不同参数） ----
// 用指针语义：开关关掉就删掉字段（= 跟随全局），打开才写值。
// 后端把 nil 当"没配"，0 当"显式设为 0"，所以这里不能存空对象占位。
function voiceOn(key) {
  return editing.value?.voice?.[key] !== undefined && editing.value?.voice?.[key] !== null
}
function setVoice(key, on, def) {
  if (!editing.value) return
  if (!editing.value.voice) editing.value.voice = {}
  if (on) editing.value.voice[key] = def
  else delete editing.value.voice[key]
}
const voiceSpeedOn = computed(() => voiceOn('speed'))
const voiceRunesOn = computed(() => voiceOn('max_sentence_runes'))
const voiceSentencesOn = computed(() => voiceOn('max_sentences'))
const voiceTimeoutOn = computed(() => voiceOn('timeout_sec'))

// ---- 这个助手的音色 ----
//
// 三项都是"留空跟随全局"：只填音色就能让同一个账号发出不同声音，
// 只有确实要用另一个平台/另一个账号时才动供应商。
const ttsDetails = computed(() => state.meta.tts_provider_details || [])
const globalProvider = computed(() => state.draft?.engine?.tts_provider || 'sherpa')
const effectiveProvider = computed(() => editing.value?.tts_provider || globalProvider.value)

const profileProviderInfo = computed(() =>
  ttsDetails.value.find((p) => p.name === effectiveProvider.value),
)

// 这个平台能选的全部音色：内置精选 + 手工清单 + 在线完整清单（含复刻）。
// 和「引擎与模型」页共用同一个接口，服务端会合并好，这里只做去重。
const pulledVoices = ref([])
const voicesLoading = ref(false)

const profileVoices = computed(() => {
  const out = [...(profileProviderInfo.value?.voices || []), ...pulledVoices.value]
  const seen = new Set()
  return out.filter((v) => {
    if (!v.id || seen.has(v.id)) return false
    seen.add(v.id)
    return true
  })
})

async function loadProfileVoices() {
  const p = effectiveProvider.value
  if (!p || p === 'none' || p === 'sherpa') return
  voicesLoading.value = true
  try {
    const r = await api.ttsVoices(p)
    pulledVoices.value = r.voices || []
  } catch {
    /* 拉不到就只用内置清单，不打断编辑 */
  } finally {
    voicesLoading.value = false
  }
}

function setProvider(v) {
  if (!editing.value) return
  if (!v) delete editing.value.tts_provider
  else editing.value.tts_provider = v
  // 换平台后原来的音色 ID 一定不适用，清掉让它回落到新平台的默认值
  delete editing.value.tts_speaker
  // 同步「声音来源」：还留着音箱原生 TTS 的话，这个助手的音色配了也不会生效
  if (v && v !== 'none' && state.draft?.voice?.backend === 'device') {
    state.draft.voice.backend = 'local'
    toast('已把「声音来源」切回本地合成，否则这个助手的音色不会生效', 'ok', 5000)
  }
  loadProfileVoices()
}

function close() {
  // 取消/点遮罩：什么都不写。新增的局部草稿直接丢掉，
  // 不会触发自动保存，也就不会凭空多出一个空档案。
  editing.value = null
  editingIdx.value = -1
  isNew.value = false
}

// 校验：这些问题如果放过去，热更新会失败，不如提前拦住
const extraHint = computed(() =>
  extraErr.value
    ? '格式错误：' + extraErr.value
    : '直接并入请求体，例如 {"enable_thinking": false}。供应商不支持的参数会被自动降级。',
)

const problems = computed(() => {
  const p = editing.value
  if (!p) return []
  const out = []
  if (!p.id?.trim()) out.push('档案 id 不能为空')
  if (p.id && profiles.value.some((x, i) => i !== editingIdx.value && x.id === p.id))
    out.push('档案 id 与其他档案重复')
  if (!p.llm?.base_url?.trim()) out.push('模型地址不能为空')
  if (!p.llm?.model?.trim()) out.push('模型名不能为空')
  if (!p.wake_words?.length) out.push('至少需要一个唤醒词')
  // 后端会校验唤醒词全局唯一；复制档案时最容易撞上，提前拦住比保存失败好懂
  const used = new Set()
  profiles.value.forEach((x, i) => {
    if (i === editingIdx.value) return
    ;(x.wake_words || []).forEach((w) => used.add(String(w).trim()))
  })
  if ((p.wake_words || []).some((w) => used.has(String(w).trim()))) {
    out.push('唤醒词与其他档案重复，请改一个专属于这个助手的词')
  }
  if (extraErr.value) out.push('扩展参数不是合法 JSON')
  return out
})

function addWake() {
  const w = wakeInput.value.trim()
  if (!w) return
  if (!editing.value.wake_words) editing.value.wake_words = []
  if (!editing.value.wake_words.includes(w)) editing.value.wake_words.push(w)
  wakeInput.value = ''
}
function rmWake(w) {
  editing.value.wake_words = editing.value.wake_words.filter((x) => x !== w)
}
function addExit() {
  const w = exitInput.value.trim()
  if (!w) return
  if (!editing.value.exit_words) editing.value.exit_words = []
  if (!editing.value.exit_words.includes(w)) editing.value.exit_words.push(w)
  exitInput.value = ''
}
function rmExit(w) {
  editing.value.exit_words = editing.value.exit_words.filter((x) => x !== w)
}

function toggleTool(name) {
  const list = editing.value.tools || []
  if (name === 'none') {
    editing.value.tools = list.includes('none') ? [] : ['none']
    return
  }
  const next = list.filter((x) => x !== 'none')
  editing.value.tools = next.includes(name) ? next.filter((x) => x !== name) : [...next, name]
}

function toolOn(name) {
  return (editing.value?.tools || []).includes(name)
}

watch(extraText, (v) => {
  nextTick(fitBoxes)
  if (!editing.value) return
  if (!v.trim()) {
    editing.value.llm.extra = null
    extraErr.value = ''
    return
  }
  try {
    editing.value.llm.extra = JSON.parse(v)
    extraErr.value = ''
  } catch (e) {
    extraErr.value = e.message
  }
})

// 系统提示词改一次就重量一次高度，免得越写越多时又变成"半行被切"
watch(
  () => editing.value?.system_prompt,
  () => nextTick(fitBoxes),
)

function apply() {
  if (problems.value.length) {
    toast(problems.value[0], 'bad', 4000)
    return
  }
  const p = editing.value
  if (!p.name?.trim()) p.name = p.id
  // 空的 voice 对象没有意义（后端会当成"全部跟随全局"），
  // 但存着会让配置文件多一行噪音，也让人误以为配过东西
  if (p.voice && Object.keys(p.voice).every((k) => !p.voice[k])) delete p.voice
  else if (p.voice && !p.voice.backend) delete p.voice.backend

  if (!state.draft.profiles) state.draft.profiles = []
  const adding = isNew.value || editingIdx.value < 0 || editingIdx.value >= state.draft.profiles.length
  if (adding) state.draft.profiles.push(p)
  else state.draft.profiles[editingIdx.value] = p
  close()
  // 不在这里弹「已保存」：写进 draft 只是让它进入自动保存队列，
  // 真正的成功/失败由顶栏状态胶囊统一报告，避免"提示已保存但其实没存上"。
}

function addProfile() {
  // 只在抽屉里造一份局部草稿：以前这里直接 push 到 state.draft，
  // 自动保存会立刻把它写盘，用户点「取消」也会留下一个空档案。
  const ids = new Set(profiles.value.map((x) => x.id))
  let n = profiles.value.length + 1
  while (ids.has(`agent${n}`)) n++
  const p = {
    id: `agent${n}`,
    name: '新助手',
    wake_words: ['新唤醒词'],
    system_prompt: '你是一个家庭语音助手。回答要口语化、简短，不超过 60 字。',
    llm: { base_url: '', api_key: '', model: '', temperature: 0.6, max_tokens: 512 },
    tools: [],
    prefetch: { time: true, weather: true, devices: true },
  }
  open(p, -1)
  isNew.value = true
}

function removeProfile(idx) {
  if (state.draft.profiles.length <= 1) {
    toast('至少要保留一个档案', 'warn')
    return
  }
  const p = state.draft.profiles[idx]
  if (!confirm(`删除档案「${p.name || p.id}」？删除后立即生效。`)) return
  state.draft.profiles.splice(idx, 1)
  toast('已删除', 'ok')
}

// 复制一份完整设置，作为「新增」的局部草稿打开抽屉。
// 不直接写进 state.draft：唤醒词必须唯一，得让用户先改一个不冲突的词再点完成。
function copyFrom(src, note) {
  const p = clone(src)
  const ids = new Set(profiles.value.map((x) => x.id))
  let base = `${p.id}-copy`
  let id = base
  let n = 2
  while (ids.has(id)) id = `${base}${n++}`
  p.id = id

  const names = new Set(profiles.value.map((x) => x.name))
  let name = `${p.name || '助手'} 副本`
  n = 2
  while (names.has(name)) name = `${p.name || '助手'} 副本 ${n++}`
  p.name = name

  open(p, -1)
  isNew.value = true
  toast(`${note}：设置已带过来，请改一个不重复的唤醒词再点完成`, 'ok', 6000)
}

function duplicate(idx) {
  copyFrom(state.draft.profiles[idx], '已复制助手')
}

// 抽屉里直接以「当前正在编辑的这份设置」为模板新建，包含未保存的改动。
function duplicateCurrent() {
  if (!editing.value) return
  // 只跟打开抽屉时的快照比，别跟 state.draft 比：后者缺省字段没补齐，
  // 会让"什么都没改"也报成"有未保存改动"。
  const dirty = baseline.value !== null && JSON.stringify(editing.value) !== baseline.value
  copyFrom(
    editing.value,
    dirty ? '已按当前编辑内容复制（原档案未保存的改动记得先点完成）' : '已按当前设置复制',
  )
}

const toolSummary = (p) => {
  const t = p.tools || []
  if (!t.length) return '全部工具'
  if (t.includes('none')) return '不使用工具'
  return t.map((n) => toolLabels[n] || n).join('、')
}
</script>

<template>
  <div class="page-frag">
    <div class="notice info">
      <Ico name="users" />
      <div>
        一个档案 = 一套独立的模型、提示词与工具。唤醒词命中哪个档案，这一轮就用它的配置。
        改完自动保存，唤醒词会推送到在线设备，不用再登音箱改文件。
      </div>
    </div>

    <!-- 唤醒词同步 -->
    <div class="card">
      <div class="card-head">
        <Ico name="wave" />
        <div class="grow">
          <div class="card-title">唤醒词同步</div>
          <div class="card-desc">
            所有档案的唤醒词会汇总成一份清单下发到设备；设备按版本号跳过重复写入
          </div>
        </div>
        <span class="tag mono">rev {{ state.kws.rev || '—' }}</span>
        <button class="btn sm" :disabled="syncing" @click="syncAll()">
          <Ico name="refresh" :size="13" />
          {{ syncing ? '同步中…' : '同步到全部设备' }}
        </button>
      </div>
      <div class="card-body" style="gap: 10px">
        <Field label="当前清单" :hint="`共 ${state.kws.count} 个，去重后按档案顺序排列`">
          <div class="chips">
            <span v-for="w in state.kws.words" :key="w" class="chip on">{{ w }}</span>
            <span v-if="!state.kws.words.length" class="chip">还没有唤醒词</span>
          </div>
        </Field>

        <div class="list" style="margin: 0 -16px">
          <div v-for="d in state.kws.devices" :key="d.id" class="row">
            <span class="dot" :class="!d.online ? '' : d.ok ? 'on' : 'err'"></span>
            <div class="row-main">
              <div class="flex items-center gap-6">
                <span class="strong small mono">{{ d.id }}</span>
                <span class="tag" :class="devTag(d).kind">{{ devTag(d).text }}</span>
                <span v-if="!d.online" class="tag">离线</span>
              </div>
              <div class="tiny mute mono mt-2 truncate">
                {{ d.path || '未知路径' }}<template v-if="d.at"> · {{ fmtAgo(d.at) }}</template>
                <template v-if="d.err"> · {{ d.err }}</template>
              </div>
            </div>
            <button class="btn sm ghost" :disabled="!d.online || syncing" @click="syncOne(d.id)">
              重新同步
            </button>
          </div>
          <div v-if="!state.kws.devices.length" class="empty" style="padding: 18px">
            <div class="tiny">还没有设备连接过</div>
          </div>
        </div>
      </div>
    </div>

    <div class="grid-2">
      <div
        v-for="(p, i) in profiles"
        :key="p.id"
        class="pcard"
        @click="open(p, i)"
      >
        <div class="pcard-head">
          <div class="pcard-avatar">{{ (p.name || 'A').slice(0, 1) }}</div>
          <div class="grow">
            <div class="flex items-center gap-6">
              <span class="strong truncate">{{ p.name || p.id }}</span>
              <span class="tag mono">{{ p.id }}</span>
            </div>
            <div class="tiny mute truncate">{{ p.llm?.model || '未填模型' }}</div>
          </div>
          <div class="flex gap-4" @click.stop>
            <button class="btn sm ghost icon" title="复制" @click="duplicate(i)">
              <Ico name="copy" :size="12" />
            </button>
            <button class="btn sm ghost icon" title="删除" @click="removeProfile(i)">
              <Ico name="trash" :size="12" />
            </button>
          </div>
        </div>

        <div class="chips">
          <span v-for="w in p.wake_words" :key="w" class="chip on">{{ w }}</span>
          <span v-if="!p.wake_words?.length" class="chip">没有唤醒词</span>
        </div>

        <div class="flex items-center gap-6 wrap">
          <span class="tag" :class="p.tools?.includes('none') ? '' : 'info'">{{
            toolSummary(p)
          }}</span>
          <span v-if="p.prefetch" class="tag violet">
            预取
            {{
              [
                p.prefetch.time && '时间',
                p.prefetch.weather && '天气',
                p.prefetch.devices && '设备',
              ]
                .filter(Boolean)
                .join('/') || '无'
            }}
          </span>
          <span class="tag mono">{{ p.llm?.base_url ? '已配地址' : '缺地址' }}</span>
        </div>
      </div>

      <button class="pcard" style="align-items: center; justify-content: center; min-height: 128px" @click="addProfile()">
        <div class="pcard-avatar">
          <Ico name="plus" :size="14" />
        </div>
        <div class="small dim">新增助手档案</div>
        <div class="tiny mute">例如：闲聊助手、翻译助手</div>
      </button>
    </div>

    <!-- 编辑抽屉 -->
    <template v-if="editing">
      <div class="drawer-mask" @click="close()"></div>
      <aside class="drawer">
        <div class="drawer-head">
          <div class="pcard-avatar">{{ (editing.name || 'A').slice(0, 1) }}</div>
          <div class="grow">
            <div class="card-title">{{ editing.name || '新助手' }}</div>
            <div class="tiny mute mono">{{ editing.id }}</div>
          </div>
          <button class="btn sm ghost" @click="close()"><Ico name="close" :size="13" /></button>
        </div>

        <div class="drawer-body">
          <div class="grid-2">
            <Field label="档案 id" hint="唯一标识，用于日志与记录，改动后等同新建">
              <input v-model="editing.id" class="input mono" placeholder="default" />
            </Field>
            <Field label="显示名称" hint="会写进系统提示词，助手被问「你是谁」时据此自称">
              <input v-model="editing.name" class="input" placeholder="管家" />
              <label class="switch mt-6">
                <input type="checkbox" v-model="editing.name_in_prompt" />
                <span class="track"></span>
                <span class="switch-label">
                  写进提示词（问「你是谁」会答「我是{{ editing.name || '…' }}」）
                </span>
              </label>
            </Field>
          </div>

          <Field
            label="唤醒词"
            hint="命中任一唤醒词就用这个档案；音箱端 keywords.txt 里要有对应词"
          >
            <div class="chips">
              <span v-for="w in editing.wake_words" :key="w" class="chip on">
                {{ w }}
                <span style="cursor: pointer; opacity: 0.7" @click="rmWake(w)">×</span>
              </span>
            </div>
            <div class="input-group mt-6">
              <input
                v-model="wakeInput"
                class="input"
                placeholder="输入唤醒词后回车，如：小黑"
                @keyup.enter="addWake()"
              />
              <button class="btn" @click="addWake()">添加</button>
            </div>
          </Field>

          <div class="card">
            <div class="card-head">
              <Ico name="cpu" />
              <div class="card-title">模型</div>
            </div>
            <div class="card-body">
              <Field label="接口地址" hint="OpenAI 兼容的 /chat/completions 上游，如 https://…/v1">
                <input
                  v-model="editing.llm.base_url"
                  class="input mono"
                  placeholder="https://tokenhub.tencentmaas.com/v1"
                />
              </Field>
              <Field label="API Key" hint="留空则不带鉴权头；建议用环境变量在服务端注入">
                <input
                  v-model="editing.llm.api_key"
                  class="input mono"
                  type="password"
                  placeholder="sk-…"
                  autocomplete="off"
                />
              </Field>
              <Field label="模型名">
                <input
                  v-model="editing.llm.model"
                  class="input mono"
                  placeholder="deepseek/deepseek-flash"
                />
              </Field>
              <div class="grid-3">
                <Field label="温度" hint="低=稳，高=活">
                  <input
                    v-model.number="editing.llm.temperature"
                    class="input mono"
                    type="number"
                    step="0.1"
                    min="0"
                    max="2"
                  />
                </Field>
                <Field label="最大长度">
                  <input
                    v-model.number="editing.llm.max_tokens"
                    class="input mono"
                    type="number"
                    min="64"
                    step="64"
                  />
                </Field>
                <Field label="超时（秒）">
                  <input
                    v-model.number="editing.llm.timeout_sec"
                    class="input mono"
                    type="number"
                    min="5"
                    step="5"
                  />
                </Field>
              </div>
              <Field
                label="扩展参数"
                tag="JSON"
                tag-kind="violet"
                :hint="extraHint"
                :hint-kind="extraErr ? 'warn' : ''"
              >
                <textarea
                  ref="extraBox"
                  v-model="extraText"
                  class="textarea mono fit"
                  rows="4"
                  placeholder='{"enable_thinking": false}'
                ></textarea>
              </Field>
            </div>
          </div>

          <Field
            label="系统提示词"
            hint="决定说话风格。语音场景务必要求：口语、简短、不要 markdown"
          >
            <textarea
              ref="promptBox"
              v-model="editing.system_prompt"
              class="textarea fit"
              rows="7"
            ></textarea>
          </Field>

          <Field label="可用工具" hint="关掉的工具模型连定义都看不到，能明显降首字延迟">
            <div class="chips">
              <span
                v-for="n in toolNames"
                :key="n"
                class="chip"
                :class="{ on: toolOn(n) }"
                @click="toggleTool(n)"
              >
                {{ toolLabels[n] || n }}
              </span>
              <span class="chip" :class="{ on: toolOn('none') }" @click="toggleTool('none')">
                不使用任何工具
              </span>
            </div>
          </Field>

          <Field
            label="预取信息"
            hint="这些信息直接塞进提示词，等于零轮工具调用——比做成工具快一个数量级"
          >
            <div class="flex gap-12 wrap">
              <label class="switch">
                <input type="checkbox" v-model="editing.prefetch.time" />
                <span class="track"></span>
                <span class="switch-label">当前时间</span>
              </label>
              <label class="switch">
                <input type="checkbox" v-model="editing.prefetch.weather" />
                <span class="track"></span>
                <span class="switch-label">天气实况</span>
              </label>
              <label class="switch">
                <input type="checkbox" v-model="editing.prefetch.devices" />
                <span class="track"></span>
                <span class="switch-label">相关设备</span>
              </label>
            </div>
          </Field>

          <Field label="退出词" hint="说到这些词就结束连续对话，默认：退出、退下、再见、停止">
            <div class="chips">
              <span v-for="w in editing.exit_words || []" :key="w" class="chip on">
                {{ w }}
                <span style="cursor: pointer; opacity: 0.7" @click="rmExit(w)">×</span>
              </span>
            </div>
            <div class="input-group mt-6">
              <input
                v-model="exitInput"
                class="input"
                placeholder="输入退出词后回车"
                @keyup.enter="addExit()"
              />
              <button class="btn" @click="addExit()">添加</button>
            </div>
          </Field>

          <div class="grid-2">
            <Field label="唤醒应答" hint="只喊唤醒词时的回应；留空则用「音色 → 固定应答」里的全局值">
              <input v-model="editing.wake_reply" class="input" placeholder="我在" />
            </Field>
            <Field label="退出应答" hint="说退出词时的回应；留空则用全局值">
              <input v-model="editing.exit_reply" class="input" placeholder="好，有事再叫我" />
            </Field>
          </div>

          <!-- 这个助手专用的发声参数：不打开就跟随全局 -->
          <Field label="这个助手的发声参数" hint="不打开的项跟随全局设置；打开后只用在这个助手上">
            <div class="subcard">
              <div class="voicerows">
                <Field
                  label="音色"
                  hint="留空跟随全局。只换音色不用改供应商——同一个账号可以有好几个声音。"
                >
                  <select class="input" :value="editing.tts_provider || ''" @change="setProvider($event.target.value)">
                    <option value="">跟随全局（{{ globalProvider }}）</option>
                    <option v-for="p in ttsDetails.filter((x) => x.name !== 'none')" :key="p.name" :value="p.name">
                      {{ p.name }} — {{ p.desc }}
                    </option>
                  </select>
                  <VoiceSelect
                    v-model="editing.tts_speaker"
                    :voices="profileVoices"
                    placeholder="跟随全局音色"
                  />
                  <div v-if="voicesLoading" class="tiny mute mt-6">正在拉取音色清单…</div>
                </Field>

                <div class="flex items-center justify-between">
                  <div class="small">语速</div>
                  <div class="flex items-center gap-8">
                    <span v-if="voiceSpeedOn" class="slider-value">
                      {{ Number(editing.voice.speed).toFixed(2) }}
                    </span>
                    <label class="switch">
                      <input
                        type="checkbox"
                        :checked="voiceSpeedOn"
                        @change="setVoice('speed', $event.target.checked, 1.0)"
                      />
                      <span class="track"></span>
                    </label>
                  </div>
                </div>
                <input
                  v-if="voiceSpeedOn"
                  type="range"
                  min="0.5"
                  max="2"
                  step="0.05"
                  v-model.number="editing.voice.speed"
                />

                <div class="flex items-center justify-between">
                  <div class="small">切句长度</div>
                  <div class="flex items-center gap-8">
                    <span v-if="voiceRunesOn" class="slider-value">
                      {{ editing.voice.max_sentence_runes }} 字
                    </span>
                    <label class="switch">
                      <input
                        type="checkbox"
                        :checked="voiceRunesOn"
                        @change="setVoice('max_sentence_runes', $event.target.checked, 20)"
                      />
                      <span class="track"></span>
                    </label>
                  </div>
                </div>
                <input
                  v-if="voiceRunesOn"
                  type="range"
                  min="6"
                  max="40"
                  step="1"
                  v-model.number="editing.voice.max_sentence_runes"
                />

                <div class="flex items-center justify-between">
                  <div class="small">单轮播报句数上限</div>
                  <div class="flex items-center gap-8">
                    <span v-if="voiceSentencesOn" class="slider-value">
                      {{ editing.voice.max_sentences === 0 ? '不限' : editing.voice.max_sentences + ' 句' }}
                    </span>
                    <label class="switch">
                      <input
                        type="checkbox"
                        :checked="voiceSentencesOn"
                        @change="setVoice('max_sentences', $event.target.checked, 6)"
                      />
                      <span class="track"></span>
                    </label>
                  </div>
                </div>
                <input
                  v-if="voiceSentencesOn"
                  type="range"
                  min="0"
                  max="12"
                  step="1"
                  v-model.number="editing.voice.max_sentences"
                />

                <div class="flex items-center justify-between">
                  <div class="small">静音多久结束对话</div>
                  <div class="flex items-center gap-8">
                    <span v-if="voiceTimeoutOn" class="slider-value">
                      {{ editing.voice.timeout_sec }} 秒
                    </span>
                    <label class="switch">
                      <input
                        type="checkbox"
                        :checked="voiceTimeoutOn"
                        @change="setVoice('timeout_sec', $event.target.checked, 20)"
                      />
                      <span class="track"></span>
                    </label>
                  </div>
                </div>
                <input
                  v-if="voiceTimeoutOn"
                  type="range"
                  min="5"
                  max="120"
                  step="1"
                  v-model.number="editing.voice.timeout_sec"
                />

                <Field label="声音来源" hint="用音箱自己的发音更好听，但不支持调速；跟全局一致就不选">
                  <select class="input" v-model="editing.voice.backend">
                    <option value="">跟随全局</option>
                    <option value="local">本地合成</option>
                    <option value="device">音箱原生 TTS</option>
                  </select>
                </Field>
              </div>
            </div>
          </Field>
        </div>

        <div class="drawer-foot">
          <div class="grow">
            <span v-if="problems.length" class="tiny" style="color: var(--danger)">
              {{ problems[0] }}
            </span>
          </div>
          <button class="btn ghost" title="以当前设置新建一份助手" @click="duplicateCurrent()">
            <Ico name="copy" :size="12" />
            复制此助手
          </button>
          <button class="btn" @click="close()">{{ isNew ? '放弃新增' : '取消' }}</button>
          <button class="btn primary" :disabled="!!problems.length" @click="apply()">
            完成
          </button>
        </div>
      </aside>
    </template>
  </div>
</template>
