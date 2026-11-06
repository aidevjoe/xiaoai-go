// 全局状态：配置、实时数据、主题、保存状态。
//
// 这一版把「先编辑再点保存」改成了「改完就存」：
//   - 任何字段变化都会进保存队列（700ms 防抖），顶栏的胶囊即时显示结果
//   - 保存期间又改的东西不会被吞掉：本轮结束后立刻再存一次
//   - 失败不丢草稿，胶囊变成可点的「重试」，错误原因展开可见
//
// 这样做的前提是「草稿 = 服务器的期望状态」而不是「待提交的表单」。
// 所以 saved 只用于两件事：判断有没有真的变化、以及「还原」。
import { reactive, computed, watch } from 'vue'
import { api, connectStream } from './api'

const clone = (v) => JSON.parse(JSON.stringify(v))

const THEME_KEY = 'xiaoai-theme'
const PAGE_KEY = 'xiaoai-page'

function loadTheme() {
  try {
    const t = localStorage.getItem(THEME_KEY)
    if (t === 'light' || t === 'dark' || t === 'auto') return t
  } catch {
    /* 隐私模式里 localStorage 会抛，忽略即可 */
  }
  return 'auto'
}

export const state = reactive({
  loading: true,
  loadError: '',
  saved: null, // 服务器上的配置（判断脏 + 还原用）
  draft: null, // 当前配置，任何改动都会自动保存
  meta: {},
  stats: {},
  devices: [],
  turns: [],
  kws: { words: [], rev: '', count: 0, devices: [] },
  toasts: [],
  streamOk: false,

  // 保存状态机：idle | pending | saving | saved | error
  saveState: 'idle',
  saveError: '',
  lastSavedAt: 0,

  theme: loadTheme(),
  page: 'dashboard',
  section: '',
  query: '', // 顶栏设置搜索
})

let seq = 0
export function toast(text, kind = 'ok', ms = 3200) {
  const id = ++seq
  state.toasts.push({ id, text, kind })
  setTimeout(() => {
    const i = state.toasts.findIndex((t) => t.id === id)
    if (i >= 0) state.toasts.splice(i, 1)
  }, ms)
}

// ---------- 主题 ----------
// auto = 不写 data-theme，交给 CSS 的 prefers-color-scheme；
// 这样系统换主题时页面会自己跟着变，不需要监听媒体查询。
export function applyTheme(t) {
  const next = t === 'light' || t === 'dark' ? t : 'auto'
  state.theme = next
  try {
    localStorage.setItem(THEME_KEY, next)
  } catch {
    /* 存不了就只在本次会话生效 */
  }
  const root = document.documentElement
  if (next === 'auto') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', next)

  const dark = next === 'dark' || (next === 'auto' && prefersDark())
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute('content', dark ? '#080b11' : '#f4f6f8')
}

function prefersDark() {
  return typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: dark)').matches
}

// ---------- 加载 ----------
export async function loadAll() {
  state.loading = true
  state.loadError = ''
  try {
    const [cfg, meta, kws] = await Promise.all([api.settings(), api.meta(), api.kws()])
    // 从服务器载入不是"用户的改动"：跳过这一次 watcher，
    // 否则每次打开页面都会先闪一下"待保存"再变"已保存"。
    skipWatch()
    state.saved = cfg
    state.draft = clone(cfg)
    state.meta = meta
    state.kws = kws
    state.saveState = 'saved'
    state.lastSavedAt = Date.now()
  } catch (e) {
    state.loadError = e.message
  } finally {
    state.loading = false
  }
  refreshLive()
}

export async function refreshLive() {
  const [st, dev, turns, kws] = await Promise.allSettled([
    api.stats(),
    api.devices(),
    api.turns(80),
    api.kws(),
  ])
  if (st.status === 'fulfilled') state.stats = st.value
  if (dev.status === 'fulfilled') state.devices = dev.value.devices || []
  if (kws.status === 'fulfilled') state.kws = kws.value
  if (turns.status === 'fulfilled') {
    // 后端按时间正序返回，界面倒序展示
    state.turns = (turns.value.turns || []).slice().reverse()
  }
}

// ---------- 即时保存 ----------
const SAVE_DEBOUNCE = 700

let timer = null
let inFlight = false
let changedWhileSaving = false

// 程序自己改 draft 时不该触发保存（从服务器载入、还原都属于这一类）。
//
// 注意不能用"同步的 suppress 变量"：watch 的回调是异步刷新的，
// 等它跑起来 suppress 早就被复位了——这就是"每次打开页面先闪一下待保存"的原因。
// 改成计数：谁来替换 draft，谁就把这个计数 +1，watcher 消费掉一次就跳过。
let skipNext = 0

function skipWatch(n = 1) {
  skipNext += n
}

function same(a, b) {
  return JSON.stringify(a) === JSON.stringify(b)
}

export function scheduleSave(delay = SAVE_DEBOUNCE) {
  if (state.loading) return
  state.saveState = 'pending'
  clearTimeout(timer)
  timer = setTimeout(flushSave, delay)
}

async function flushSave() {
  clearTimeout(timer)
  timer = null
  if (!state.draft || !state.saved) return
  if (same(state.draft, state.saved)) {
    state.saveState = 'saved'
    return
  }
  if (inFlight) {
    // 上一轮还没回来：等它结束后自动补一轮，不要并发 PUT
    changedWhileSaving = true
    return
  }
  inFlight = true
  state.saveState = 'saving'
  state.saveError = ''
  const snapshot = clone(state.draft)
  try {
    const r = await api.saveSettings(snapshot)
    state.saved = snapshot
    state.lastSavedAt = Date.now()
    state.saveState = 'saved'
    if (r.kws_dirty) {
      // 唤醒词变更会自动重载引擎（服务端做），这里只提示结果
      toast('唤醒词已保存，正在同步到设备并重载引擎（几秒内生效）', 'ok', 5000)
    } else if (r.engine_dirty) {
      toast('模型参数已保存，点「重载」后生效', 'warn', 5000)
    }
    refreshLive()
  } catch (e) {
    // 不还原草稿：用户刚写的东西不能因为一次网络抖动就消失
    state.saveState = 'error'
    state.saveError = e.message
    toast('保存失败：' + e.message + '（点右上角胶囊重试）', 'bad', 7000)
  } finally {
    inFlight = false
    if (changedWhileSaving) {
      changedWhileSaving = false
      scheduleSave(0)
    }
  }
}

// 立即保存（⌘S / 点胶囊）
export function saveNow() {
  if (state.saveState === 'error') state.saveError = ''
  return flushSave()
}

export function revert() {
  if (!state.saved) return
  skipWatch()
  state.draft = clone(state.saved)
  state.saveState = 'saved'
  toast('已还原为服务器上的配置', 'ok')
}

// 只还原某一段（引擎/声音/对话…）：改错一片时不用整份重来
export function revertSection(key) {
  if (!state.saved || !state.draft) return
  skipWatch()
  state.draft[key] = clone(state.saved[key])
  state.saveState = 'saved'
  toast('已把这一段还原为服务器上的配置', 'ok')
}

// 外部导入整份配置：写进 draft 后走正常保存流程
export function replaceDraft(obj) {
  state.draft = obj // 这一份是要保存的，不能跳过 watcher
  scheduleSave(0)
}

// 深度监听整份配置：任何字段（含数组、嵌套对象）改动都会进保存队列。
// 相比给每个 v-model 挂 @change，这样页面代码不用关心保存，新增字段也不会漏。
watch(
  () => state.draft,
  () => {
    if (skipNext > 0) {
      skipNext--
      return
    }
    scheduleSave()
  },
  { deep: true },
)

export const dirty = computed(() => {
  if (!state.saved || !state.draft) return false
  return !same(state.saved, state.draft)
})

// 保存状态的人类可读文案
export const saveLabel = computed(() => {
  switch (state.saveState) {
    case 'pending':
      return '待保存'
    case 'saving':
      return '保存中…'
    case 'error':
      return '保存失败'
    case 'saved':
      return state.lastSavedAt ? '已保存' : '已同步'
    default:
      return '已同步'
  }
})

export function startLive() {
  const stop = connectStream((ev) => {
    if (ev.type === 'stream') {
      state.streamOk = !!ev.data.ok
      return
    }
    if (ev.type === 'turn') {
      state.turns.unshift(ev.data)
      if (state.turns.length > 120) state.turns.length = 120
      return
    }
    if (ev.type === 'settings') {
      refreshLive()
    }
  })
  const timer = setInterval(refreshLive, 5000)
  return () => {
    stop()
    clearInterval(timer)
  }
}

// ---------- 路由 ----------
// 页面 + 页内分区（#voice/speak）。分区让"语音"这种大页也能深链到具体一组设置。
export function go(page, section = '') {
  state.page = page
  state.section = section
  const hash = section ? `${page}/${section}` : page
  if (location.hash.replace('#', '') !== hash) location.hash = hash
  try {
    localStorage.setItem(PAGE_KEY, hash)
  } catch {
    /* ignore */
  }
}

export function parseHash() {
  const raw = location.hash.replace('#', '')
  if (!raw) {
    try {
      return localStorage.getItem(PAGE_KEY) || ''
    } catch {
      return ''
    }
  }
  return raw
}

export { clone }
