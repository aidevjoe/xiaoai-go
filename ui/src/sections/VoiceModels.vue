<script setup>
// 语音 · 引擎与模型
// 回答一个问题：「识别和合成，分别是谁在做、模型在哪、吃几个核」。
//
// 合成这一块是"通用接入层"的正面：供应商的配置项、音色清单、复刻能力
// 全部由服务端声明（/api/meta 的 tts_provider_details），这里只负责渲染。
// 加一家新平台，这个文件一行都不用改。
import { computed, onUnmounted, ref, watch } from 'vue'
import Ico from '../components/Ico.vue'
import Field from '../components/Field.vue'
import TTSFields from '../components/TTSFields.vue'
import VoiceSelect from '../components/VoiceSelect.vue'
import { state, toast, refreshLive } from '../store'
import { api } from '../api'
import { fmtBytes } from '../utils'

const e = computed(() => state.draft?.engine || {})
const busy = ref('')

const asrProviders = computed(() => state.meta.asr_providers || [])
const ttsProviders = computed(() => state.meta.tts_providers || [])
// 完整声明：带 fields / voices / clone。老后端没有这个字段时退回空数组。
const ttsDetails = computed(() => state.meta.tts_provider_details || [])
const findProvider = (list, name) => (list || []).find((p) => p.name === (name || ''))
const asrIsRemote = computed(
  () => !(findProvider(asrProviders.value, e.value.asr_provider)?.local ?? true),
)
const curProvider = computed(() => findProvider(ttsDetails.value, e.value.tts_provider))
// 只有"选中的平台真的有可配的东西"才展开配置块。
//
// 判断依据是供应商自己的声明（fields / voices / clone），不是名字白名单：
// 以后接一家新平台，只要它声明了配置项，这里自动就展开，不用改前端。
// sherpa 的配置在下面的「模型位置」里；none 表示关掉合成，两者都没有可配项。
const showTTSPanel = computed(() => {
  const p = e.value.tts_provider
  if (!p || p === 'none' || p === 'sherpa') return false
  const info = curProvider.value
  if (!info) return true // 声明拉不到（老后端）：宁可显示出来让人看到键值对
  const hasFields = (info.fields || []).length > 0
  const hasVoices = (info.voices || []).length > 0 || !!info.clone
  // 没声明配置项但已经填过参数：仍然显示，否则用户没法修正/清空
  const cfg = e.value.tts_providers?.[p]
  return hasFields || hasVoices || (cfg && Object.keys(cfg).length > 0)
})

// 「关掉本地合成」和「声音来源」是同一件事的两半：只关掉合成、不改声音来源，
// 服务端仍会逐句去调 TTS，结果每一句都失败、整轮没声。
watch(
  () => e.value.tts_provider,
  (name, prev) => {
    if (name === prev) return
    // 换平台时旧音色 ID 一定不适用（豆包的 zh_xxx 在 Edge 上不存在），
    // 直接清掉回落到新平台的默认音色，比留着让它合成失败好。
    if (prev !== undefined && e.value.tts_voice) {
      e.value.tts_voice = ''
      pulledVoices.value = []
      toast('已切换合成平台，音色回到该平台默认值', 'ok', 4500)
    }
    // 开了平台却还留着「音箱原生 TTS」是最常见的"配了没用"：
    // 播放走的还是设备那一路，新配的供应商完全没被调用。
    if (name && name !== 'none' && state.draft?.voice?.backend === 'device') {
      state.draft.voice.backend = 'local'
      toast('已把「声音来源」切回本地合成，否则仍会用音箱原生 TTS', 'ok', 5000)
    }
    if (name !== 'none' || prev === 'none' || !state.draft?.voice) return
    if (state.draft.voice.backend === 'device') return
    state.draft.voice.backend = 'device'
    toast('已同时把「声音来源」切换为音箱原生 TTS', 'ok', 4500)
  },
)

// ---- 每个供应商的参数：tts_providers[provider] ----
const providerCfg = computed({
  get() {
    const p = e.value.tts_provider
    if (!p) return {}
    return e.value.tts_providers?.[p] || {}
  },
  set(v) {
    const p = e.value.tts_provider
    if (!p) return
    if (!e.value.tts_providers) e.value.tts_providers = {}
    e.value.tts_providers[p] = v
  },
})

// 供应商没有声明 fields 时（第三方插件注册的），退回键值对编辑
const showRawParams = computed(
  () => showTTSPanel.value && !(curProvider.value?.fields || []).length,
)

function addParam() {
  const m = providerCfg.value
  let i = 1
  while (m[`参数${i}`] !== undefined) i++
  providerCfg.value = { ...m, [`参数${i}`]: '' }
}
function delParam(key) {
  const m = { ...providerCfg.value }
  delete m[key]
  providerCfg.value = m
}
function renameParam(oldKey, newKey) {
  newKey = (newKey || '').trim()
  if (!newKey || newKey === oldKey) return
  const m = { ...providerCfg.value }
  if (m[newKey] !== undefined) {
    toast(`参数 ${newKey} 已存在`, 'bad')
    return
  }
  m[newKey] = m[oldKey]
  delete m[oldKey]
  providerCfg.value = m
}

// ---- ASR 参数（仍然是一张扁平表）----
function asrParams() {
  if (!e.value.asr_params) e.value.asr_params = {}
  return e.value.asr_params
}
function addAsrParam() {
  const m = asrParams()
  let i = 1
  while (m[`参数${i}`] !== undefined) i++
  m[`参数${i}`] = ''
}
function delAsrParam(key) {
  delete asrParams()[key]
}
function renameAsrParam(oldKey, newKey) {
  newKey = (newKey || '').trim()
  if (!newKey || newKey === oldKey) return
  const m = asrParams()
  if (m[newKey] !== undefined) {
    toast(`参数 ${newKey} 已存在`, 'bad')
    return
  }
  m[newKey] = m[oldKey]
  delete m[oldKey]
}

// ---- 音色 ----
//
// 清单 = 内置精选 + 配置里登记的复刻音色（clone_voices）+ 在线拉取的完整清单。
// 用 datalist 而不是 select：既能在候选里挑，也能直接粘一个控制台复制的 ID。
const pulledVoices = ref([])
const pulling = ref(false)

const voiceOptions = computed(() => {
  const out = [...(curProvider.value?.voices || []), ...pulledVoices.value]
  // clone_voices 里手工登记的复刻音色
  const raw = providerCfg.value.clone_voices || ''
  for (const item of raw.split(',')) {
    const s = item.trim()
    if (!s) continue
    const i = s.search(/[:：]/)
    const id = i > 0 ? s.slice(0, i).trim() : s
    const name = i > 0 ? s.slice(i + 1).trim() : s
    if (id) out.push({ id, name: name || id, cloned: true })
  }
  const seen = new Set()
  return out.filter((v) => {
    if (!v.id || seen.has(v.id)) return false
    seen.add(v.id)
    return true
  })
})

// 服务端会把"内置精选 + 手工清单 + 在线完整清单（带缓存）"合并后返回，
// 所以这里不用自己拼，拿到什么用什么。
async function loadVoices(refresh = false) {
  const p = e.value.tts_provider
  if (!p || p === 'none' || p === 'sherpa') return
  pulling.value = true
  try {
    const r = await api.ttsVoices(p, refresh)
    pulledVoices.value = r.voices || []
    if (r.error) toast('在线清单没拿全：' + r.error, 'warn', 7000)
    else if (refresh) toast(`已刷新，共 ${pulledVoices.value.length} 个音色`, 'ok')
  } catch (err) {
    if (refresh) toast(err.message, 'bad', 6000)
  } finally {
    pulling.value = false
  }
}

// 换平台/首次进页面自动拉一次：不点按钮也能看到完整清单（走服务端缓存，很快）
watch(
  () => e.value.tts_provider,
  (p) => {
    if (p && p !== 'none' && p !== 'sherpa') loadVoices(false)
  },
  { immediate: true },
)

// 音色清单编辑器是否展开
const showVoiceList = ref(false)

function setVoiceList(v) {
  const cfg = { ...providerCfg.value }
  if (!v.trim()) delete cfg.voice_list
  else cfg.voice_list = v
  providerCfg.value = cfg
}

// ---- 试听 ----
const sampleText = ref('好的，客厅的灯已经打开了。还需要我把温度调到 26 度吗？')
const previewUrl = ref('')
const previewInfo = ref('')
const previewErr = ref('')

async function preview() {
  busy.value = 'preview'
  previewErr.value = ''
  const t0 = performance.now()
  try {
    const res = await api.ttsPreview({
      text: sampleText.value,
      provider: e.value.tts_provider || '',
      voice: e.value.tts_voice || '',
      speed: state.draft?.voice?.speed || 1,
    })
    const ct = res.headers.get('content-type') || ''
    if (!ct.includes('audio')) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error || `HTTP ${res.status}`)
    }
    const blob = await res.blob()
    if (previewUrl.value) URL.revokeObjectURL(previewUrl.value)
    previewUrl.value = URL.createObjectURL(blob)
    previewInfo.value = `${fmtBytes(blob.size)} · ${Math.round(performance.now() - t0)}ms`
  } catch (err) {
    previewErr.value = err.message
  } finally {
    busy.value = ''
  }
}

// ---- 声音复刻 ----
const cloneBusy = ref('')
const cloneSpeaker = ref('')
const cloneText = ref('')
const cloneParams = ref({})
const cloneState = ref(null)
const cloneErr = ref('')
const cloneVoices = computed(() => curProvider.value?.clone?.fields || [])
let pollTimer = null

function stopPoll() {
  clearTimeout(pollTimer)
  pollTimer = null
}

async function doClone(ev) {
  const file = ev.target.files?.[0]
  ev.target.value = '' // 允许重复选同一个文件
  if (!file) return
  if (!cloneSpeaker.value.trim()) {
    cloneErr.value = '先填音色槽位号（S_xxxxxxxx，在火山引擎控制台的音色库/声音复刻里创建）'
    return
  }
  if (!curProvider.value?.clone) {
    cloneErr.value = '当前供应商不支持声音复刻'
    return
  }
  cloneBusy.value = 'upload'
  cloneErr.value = ''
  cloneState.value = null
  stopPoll()
  try {
    const buf = await file.arrayBuffer()
    const b64 = toBase64(new Uint8Array(buf))
    const fmt = (file.name.split('.').pop() || 'wav').toLowerCase()
    const r = await api.ttsClone({
      provider: e.value.tts_provider,
      speaker_id: cloneSpeaker.value.trim(),
      format: fmt,
      text: cloneText.value,
      audio: b64,
      params: cloneParams.value,
    })
    cloneState.value = r.state
    toast('已提交训练，正在等待平台处理', 'ok', 4000)
    poll()
  } catch (err) {
    cloneErr.value = err.message
  } finally {
    cloneBusy.value = ''
  }
}

function poll() {
  stopPoll()
  const id = cloneSpeaker.value.trim()
  if (!id) return
  pollTimer = setTimeout(async () => {
    try {
      const r = await api.ttsCloneStatus(e.value.tts_provider, id)
      cloneState.value = r.state
      const st = r.state?.status
      if (st !== 'ready' && st !== 'active' && st !== 'failed' && st !== 'notfound') {
        poll()
      } else if (st === 'ready' || st === 'active') {
        toast('音色训练完成，可以用了', 'ok', 5000)
        pulledVoices.value = [...pulledVoices.value, { id, name: id, cloned: true }]
      }
    } catch (err) {
      cloneErr.value = err.message
    }
  }, 3000)
}

onUnmounted(() => {
  stopPoll()
  if (previewUrl.value) URL.revokeObjectURL(previewUrl.value)
})

// toBase64：btoa 只吃 latin1，得分块转，否则大文件会栈溢出
function toBase64(bytes) {
  let s = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk))
  }
  return btoa(s)
}

// 复刻成功后最常踩的坑：音色 ID 换成 S_xxx 了，但模型版本还是 seed-tts-2.0。
function useCloneVoice(id) {
  e.value.tts_voice = id
  const p = providerCfg.value
  if (e.value.tts_provider === 'doubao' && (!p.resource_id || p.resource_id === 'seed-tts-2.0')) {
    providerCfg.value = { ...p, resource_id: 'seed-icl-2.0' }
    toast('已把模型版本切到 seed-icl-2.0（复刻音色必须用它）', 'ok', 5000)
  }
}

// ---- 目录浏览 ----
const browseOpen = ref(false)
const browseDir = ref('')
const browseItems = ref([])
const browseErr = ref('')

async function browse(dir) {
  busy.value = 'browse'
  try {
    const r = await api.models(dir || '')
    browseDir.value = r.dir
    browseItems.value = r.items || []
    browseErr.value = r.error || ''
    browseOpen.value = true
  } catch (err) {
    toast(err.message, 'bad')
  } finally {
    busy.value = ''
  }
}
function pick(it) {
  if (it.kind === 'dir') return browse(it.path)
  e.value.model_dir = browseDir.value
  browseOpen.value = false
  toast(`模型目录设为 ${browseDir.value}`, 'ok')
}
function useAsDir() {
  e.value.model_dir = browseDir.value
  browseOpen.value = false
}

const reload = async () => {
  busy.value = 'reload'
  try {
    const r = await api.engineReload()
    toast(`引擎已重载（${r.ms}ms）· ${r.status}`, 'ok', 5000)
    await refreshLive()
  } catch (err) {
    toast(err.message, 'bad', 6000)
  } finally {
    busy.value = ''
  }
}

const engineTone = computed(() => {
  if (state.meta.no_engine) return 'warn'
  return state.stats.engine === 'ready' ? 'ok' : 'bad'
})
</script>

<template>
  <div class="sheet">
    <!-- 状态：这一页所有设置都只为"让它跑起来"服务 -->
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">引擎状态</div>
          <div class="sect-d">四个零件分别加载，其中一个没起来也不影响其余工作。</div>
        </div>
        <button class="btn sm" :disabled="busy === 'reload'" @click="reload()">
          <Ico name="refresh" :size="13" />
          {{ busy === 'reload' ? '重载中…' : '重载引擎' }}
        </button>
      </div>
      <div class="notice" :class="engineTone">
        <Ico name="cpu" />
        <div class="grow">
          <div class="strong">
            {{
              state.meta.no_engine
                ? '服务以 -no-engine 启动，引擎被禁用'
                : state.stats.engine === 'ready'
                  ? '引擎就绪'
                  : '引擎未就绪'
            }}
          </div>
          <div class="mono tiny mt-2">{{ state.stats.engine_status || state.stats.engine }}</div>
        </div>
      </div>
    </section>

    <!-- 供应商 -->
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">谁来识别、谁来合成</div>
          <div class="sect-d">候选来自服务端注册表：列出什么，就说明这个构建真的带了什么。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">识别（ASR）</div>
            <div class="row2-hint">留空用本地 sherpa；在线供应商按参数里的地址调用。</div>
          </div>
          <div class="row2-ctl">
            <select class="input" v-model="e.asr_provider">
              <option value="">默认（本地 sherpa）</option>
              <option v-for="p in asrProviders" :key="p.name" :value="p.name">
                {{ p.name }} — {{ p.desc }}
              </option>
            </select>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">合成（TTS）</div>
            <div class="row2-hint">
              选一个在线平台，下面会展开它自己的参数、音色与试听；选 none 会把「说与播报」切到音箱原生 TTS。
            </div>
          </div>
          <div class="row2-ctl">
            <select class="input" v-model="e.tts_provider">
              <option value="">默认（本地 sherpa）</option>
              <option v-for="p in ttsProviders" :key="p.name" :value="p.name">
                {{ p.name }} — {{ p.desc }}
              </option>
            </select>
          </div>
        </div>
      </div>

      <!-- 合成：只有"选中的平台真的有配置项"才展开。
           默认（本地 sherpa）/ none 没有可配的东西，展开只会多一块空壳；
           想配豆包/Edge，在上面「合成（TTS）」里选它，这块就出来。 -->
      <div v-if="showTTSPanel" class="subcard mt-10">
        <div class="subcard-head">
          <div class="grow">
            <div class="card-title">{{ curProvider?.desc || e.tts_provider }}</div>
            <div class="tiny mute">
              每个平台单独存一份参数，多个助手可以混用不同平台，不用重复填密钥。
            </div>
          </div>
          <button
            v-if="curProvider?.voices?.length || curProvider?.clone"
            class="btn sm ghost"
            :disabled="pulling"
            @click="pullVoices()"
          >
            <Ico name="refresh" :size="13" />
            {{ pulling ? '拉取中…' : '刷新音色' }}
          </button>
        </div>

        <TTSFields
          v-if="(curProvider?.fields || []).length"
          :fields="curProvider.fields"
          v-model="providerCfg"
        />

        <!-- 第三方/老供应商没声明 fields：退回键值对 -->
        <template v-else-if="showRawParams">
          <div v-for="(v, k) in providerCfg" :key="k" class="kv-row">
            <input
              class="input mono kv-key"
              :value="k"
              @change="renameParam(k, $event.target.value)"
            />
            <input
              class="input mono grow"
              :value="v"
              @input="providerCfg = { ...providerCfg, [k]: $event.target.value }"
            />
            <button class="btn sm ghost icon" @click="delParam(k)">
              <Ico name="close" :size="13" />
            </button>
          </div>
          <button class="btn sm ghost" @click="addParam()">
            <Ico name="plus" :size="13" /> 添加参数
          </button>
        </template>

        <!-- 音色：datalist 既能挑，也能直接粘平台控制台里的 ID -->
        <Field
          v-if="showTTSPanel"
          label="全局音色"
          :hint="`共 ${voiceOptions.length} 个可选。这个音箱默认用哪个声音；单个助手可以在「助手档案」里覆盖。`"
        >
          <VoiceSelect
            v-model="e.tts_voice"
            :voices="voiceOptions"
            placeholder="平台默认音色"
          />

          <div class="flex gap-6 wrap mt-8">
            <button
              v-if="curProvider?.discover"
              class="btn sm"
              :disabled="pulling"
              @click="loadVoices(true)"
            >
              <Ico name="refresh" :size="13" />
              {{ pulling ? '拉取中…' : '从平台拉取全部音色' }}
            </button>
            <button class="btn sm ghost" @click="showVoiceList = !showVoiceList">
              {{ showVoiceList ? '收起音色清单' : '自定义音色清单' }}
            </button>
          </div>

          <div v-if="showVoiceList" class="subcard mt-8">
            <div class="tiny mute">
              每行一条，写成 <span class="mono">音色ID</span> 或
              <span class="mono">音色ID:备注名</span>。豆包没有公开的"列出音色"接口，
              可以从控制台音色库复制 ID 粘进来（也可以把 Edge 拉到的结果存到这里）。
            </div>
            <textarea
              class="textarea mono mt-6"
              rows="5"
              :value="providerCfg.voice_list || ''"
              placeholder="zh_male_lengkugege_emo_v2_mars_bigtts:低沉男声"
              @input="setVoiceList($event.target.value)"
            ></textarea>
          </div>
        </Field>

        <!-- 试听：先听再存，避免"选完保存才发现难听" -->
        <div class="field">
          <div class="field-label"><span>试听</span></div>
          <textarea v-model="sampleText" class="textarea" rows="2"></textarea>
          <div class="flex items-center gap-8 wrap mt-6">
            <button class="btn primary sm" :disabled="busy === 'preview'" @click="preview()">
              <Ico name="play" :size="13" />
              {{ busy === 'preview' ? '合成中…' : '用当前音色合成' }}
            </button>
            <span class="tiny mute">{{ previewInfo }}</span>
          </div>
          <audio v-if="previewUrl" :src="previewUrl" controls style="width: 100%; height: 34px; margin-top: 8px"></audio>
          <div v-if="previewErr" class="notice bad"><Ico name="close" /><span class="mono small">{{ previewErr }}</span></div>
        </div>

        <!-- 声音复刻：只有声明了 clone 能力的供应商才出现 -->
        <div v-if="curProvider?.clone" class="subcard mt-10">
          <div class="card-title">声音复刻</div>
          <div class="tiny mute">
            上传 {{ curProvider.clone.min_seconds || 10 }}~{{ curProvider.clone.max_seconds || 30 }} 秒
            干净人声，平台训练出你自己的音色。训练是异步的，完成后要选到「全局音色」里。
          </div>

          <div class="grid-2 mt-8">
            <Field label="音色槽位号" hint="形如 S_xxxxxxxx。先在平台控制台创建/购买一个槽位。">
              <input v-model="cloneSpeaker" class="input mono" placeholder="S_xxxxxxxx" />
            </Field>
            <Field label="样本文字（可选）" hint="和录音内容一致；填了平台会校验读音是否对得上。">
              <input v-model="cloneText" class="input" placeholder="留空跳过校验" />
            </Field>
          </div>

          <TTSFields :fields="cloneVoices" v-model="cloneParams" />

          <div class="flex items-center gap-8 wrap">
            <label class="btn sm" :class="{ disabled: cloneBusy === 'upload' }">
              <Ico name="plus" :size="13" />
              {{ cloneBusy === 'upload' ? '上传中…' : '选择样本音频并开始训练' }}
              <input type="file" accept="audio/*" style="display: none" @change="doClone" />
            </label>
            <span v-if="cloneState" class="tag" :class="cloneState.status === 'ready' || cloneState.status === 'active' ? 'ok' : cloneState.status === 'failed' ? 'bad' : ''">
              {{ cloneState.status }}
            </span>
            <span v-if="cloneState?.detail" class="tiny mute">{{ cloneState.detail }}</span>
            <button
              v-if="cloneState && (cloneState.status === 'ready' || cloneState.status === 'active')"
              class="btn sm primary"
              @click="useCloneVoice(cloneState.speaker_id || cloneSpeaker)"
            >
              用这个音色
            </button>
          </div>
          <div v-if="cloneErr" class="notice bad"><Ico name="close" /><span class="small">{{ cloneErr }}</span></div>
        </div>
      </div>

      <div v-if="asrIsRemote" class="subcard mt-10">
        <div class="subcard-head"><div class="card-title">在线识别参数</div></div>
        <div v-for="(v, k) in e.asr_params" :key="'a' + k" class="kv-row">
          <input class="input mono kv-key" :value="k" @change="renameAsrParam(k, $event.target.value)" />
          <input class="input mono grow" :value="v" @input="asrParams()[k] = $event.target.value" />
          <button class="btn sm ghost icon" @click="delAsrParam(k)">
            <Ico name="close" :size="13" />
          </button>
        </div>
        <button class="btn sm ghost" @click="addAsrParam()">
          <Ico name="plus" :size="13" /> 添加参数
        </button>
      </div>
    </section>

    <!-- 模型位置 -->
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">模型位置</div>
          <div class="sect-d">留空的路径会按「模型根目录」的约定自动推导。</div>
        </div>
        <button class="btn sm ghost" :disabled="busy === 'browse'" @click="browse(e.model_dir)">
          <Ico name="grid" :size="13" /> 浏览目录
        </button>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">模型根目录</div>
            <div class="row2-hint">含 silero_vad.onnx、识别模型与合成模型目录的上级。</div>
          </div>
          <div class="row2-ctl">
            <input v-model="e.model_dir" class="input mono" placeholder="models" />
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">识别模型目录</div>
            <div class="row2-hint">含 model.int8.onnx 与 tokens.txt；留空自动发现。</div>
          </div>
          <div class="row2-ctl">
            <input v-model="e.asr_dir" class="input mono" placeholder="留空自动发现" />
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">合成模型文件</div>
            <div class="row2-hint">.onnx 路径；Matcha 还需要下面的声码器。</div>
          </div>
          <div class="row2-ctl">
            <input v-model="e.tts_model" class="input mono" placeholder="留空自动发现" />
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">识别架构</div>
            <div class="row2-hint">留空按目录名推断；选错会直接加载失败。</div>
          </div>
          <div class="row2-ctl">
            <select class="input" v-model="e.asr_kind">
              <option value="">自动（按目录名）</option>
              <option value="sensevoice">SenseVoice</option>
              <option value="paraformer">Paraformer（中文更快）</option>
            </select>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">合成架构</div>
            <div class="row2-hint">Matcha 把声学模型和声码器分开，采样率由声码器决定。</div>
          </div>
          <div class="row2-ctl">
            <select class="input" v-model="e.tts_kind">
              <option value="">自动（看有没有声码器）</option>
              <option value="vits">VITS</option>
              <option value="matcha">Matcha</option>
            </select>
          </div>
        </div>
        <div v-if="e.tts_kind === 'matcha'" class="row2">
          <div>
            <div class="row2-label">声码器</div>
            <div class="row2-hint">vocos-22khz-univ.onnx 之类。</div>
          </div>
          <div class="row2-ctl">
            <input v-model="e.tts_vocoder" class="input mono" placeholder="留空自动发现" />
          </div>
        </div>
      </div>
    </section>

    <!-- 推理与灵敏度 -->
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">推理与灵敏度</div>
          <div class="sect-d">线程数按部署设备的核数自动决定；人声阈值属于音频算法层。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">推理线程</div>
            <div class="row2-hint">0 = 自动；调低可给别的服务让路。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="0" max="8" step="1" v-model.number="e.threads" />
              <span class="slider-value">{{ e.threads === 0 ? '自动' : e.threads }}</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">人声判定阈值</div>
            <div class="row2-hint">越小越灵敏，但空调噪声也可能被当成人声。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="0.02" max="0.9" step="0.01" v-model.number="e.vad_threshold" />
              <span class="slider-value">{{ Number(e.vad_threshold).toFixed(2) }}</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">识别语种</div>
            <div class="row2-hint">固定中文比 auto 准得多：auto 会把噪声片段猜成英文/日文。</div>
          </div>
          <div class="row2-ctl">
            <select class="input" v-model="e.asr_lang">
              <option value="zh">中文（推荐）</option>
              <option value="auto">自动检测</option>
              <option value="en">English</option>
              <option value="ja">日本語</option>
              <option value="ko">한국어</option>
              <option value="yue">粤语</option>
            </select>
          </div>
        </div>
      </div>
      <div class="notice info mt-10">
        <Ico name="clock" />
        <div>
          改完这里的任何一项都要点上面的「重载引擎」才会加载新模型：期间几百毫秒不可用，内存峰值约翻一倍。
        </div>
      </div>
    </section>
  </div>

  <template v-if="browseOpen">
    <div class="drawer-mask" @click="browseOpen = false"></div>
    <aside class="drawer drawer-narrow">
      <div class="drawer-head">
        <div class="grow">
          <div class="card-title">选择目录</div>
          <div class="tiny mute mono truncate">{{ browseDir }}</div>
        </div>
        <button class="btn sm ghost" @click="browseOpen = false"><Ico name="close" :size="13" /></button>
      </div>
      <div class="drawer-body" style="padding: 0">
        <div v-if="browseErr" class="notice bad" style="margin: 14px">
          <Ico name="close" /><span class="mono small">{{ browseErr }}</span>
        </div>
        <div class="list">
          <div v-for="it in browseItems" :key="it.path" class="row" style="cursor: pointer" @click="pick(it)">
            <Ico :name="it.kind === 'dir' ? 'grid' : 'cpu'" :size="14" />
            <div class="row-main truncate">{{ it.name }}</div>
            <span v-if="it.kind === 'file'" class="tag mono">{{ fmtBytes(it.size) }}</span>
            <Ico v-else name="play" :size="12" />
          </div>
          <div v-if="!browseItems.length" class="empty"><div class="tiny">空目录</div></div>
        </div>
      </div>
      <div class="drawer-foot">
        <span class="tiny mute grow">点目录进去，点文件用它的上级目录</span>
        <button class="btn" @click="browseOpen = false">关闭</button>
        <button class="btn primary" @click="useAsDir()">用此目录</button>
      </div>
    </aside>
  </template>
</template>
