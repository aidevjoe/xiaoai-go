<script setup>
// 应用外壳：侧栏导航 + 顶栏（标题/搜索/保存状态/主题）+ 内容区。
//
// 这一版的三处结构性改动：
//   1. 导航由 modules.js 的注册表驱动，不再写死在组件里（加页面不用改这里）
//   2. 顶栏右侧常驻「保存状态胶囊」：改完即存，结果一眼可见，不再有"忘记保存"
//   3. 主题三态（跟随系统/浅色/深色），默认跟随系统
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import Ico from './components/Ico.vue'
import Logo from './components/Logo.vue'
import { MODULES, GROUPS, mergeRemoteModules, searchTargets, findModule } from './modules'
import {
  state,
  dirty,
  saveNow,
  loadAll,
  startLive,
  applyTheme,
  go,
  parseHash,
  saveLabel,
} from './store'

const modules = computed(() => mergeRemoteModules(state.meta.modules))
const visibleGroups = computed(() =>
  GROUPS.map((g) => ({ ...g, items: modules.value.filter((m) => m.group === g.id) })).filter(
    (g) => g.items.length,
  ),
)

const cur = computed(() => findModule(modules.value, state.page))
const curSection = computed(() =>
  (cur.value.sections || []).find((s) => s.id === state.section),
)
// 页内不再放大标题（之前和卡片左边界对不齐），当前位置由顶栏直接说清：
// 标题 = 分区名，副标题 = 模块名 · 分区说明。
const pageTitle = computed(() => curSection.value?.label || cur.value.label)
const pageSub = computed(() =>
  curSection.value ? `${cur.value.label} · ${curSection.value.desc}` : cur.value.sub || '',
)
const activeSectionId = computed(() => curSection.value?.id || '')

// 有分区的页面由 SectionPage 渲染，把 module 传下去
const curComp = computed(() => cur.value.component)
const compProps = computed(() =>
  (cur.value.sections || []).length ? { module: cur.value, section: state.section } : {},
)

const navOpen = ref(false)

function nav(page, section = '') {
  go(page, section)
  navOpen.value = false
  state.query = ''
  searchOpen.value = false
}

// ---------- 设置搜索 ----------
// 目标 = 模块 + 页内分区。搜"静音"能直接落到「语音 · 听与分段」，
// 不用先想它属于哪一页——这是这次 IA 调整最直接的降本点。
const searchOpen = ref(false)
const searchInput = ref(null)
const activeIdx = ref(0)

const targets = computed(() => searchTargets(modules.value))
const results = computed(() => {
  const q = state.query.trim().toLowerCase()
  if (!q) return []
  return targets.value.filter((t) => t.keywords.includes(q)).slice(0, 8)
})

function onSearchKey(e) {
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    activeIdx.value = Math.min(activeIdx.value + 1, results.value.length - 1)
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    activeIdx.value = Math.max(activeIdx.value - 1, 0)
  } else if (e.key === 'Enter') {
    const t = results.value[activeIdx.value]
    if (t) nav(t.page, t.section)
  } else if (e.key === 'Escape') {
    searchOpen.value = false
    state.query = ''
    searchInput.value?.blur()
  }
}
watch(
  () => state.query,
  () => (activeIdx.value = 0),
)

// ---------- 主题 ----------
const themes = [
  { id: 'auto', icon: 'monitor', label: '跟随系统' },
  { id: 'light', icon: 'sun', label: '浅色' },
  { id: 'dark', icon: 'moon', label: '深色' },
]
const themeLabel = computed(() => themes.find((t) => t.id === state.theme)?.label || '跟随系统')

// ---------- 保存状态 ----------
const saveTone = computed(() => {
  switch (state.saveState) {
    case 'error':
      return 'bad'
    case 'pending':
    case 'saving':
      return 'busy'
    case 'saved':
      return dirty.value ? 'busy' : 'ok'
    default:
      return ''
  }
})
const savedAtText = computed(() => {
  if (!state.lastSavedAt) return ''
  const d = new Date(state.lastSavedAt)
  const p = (n) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
})

// ---------- 路由 ----------
function applyHash() {
  const raw = parseHash()
  if (!raw) return
  const [page, section] = raw.split('/')
  const m = modules.value.find((x) => x.id === page)
  if (!m) return
  const hasSection = (m.sections || []).some((s) => s.id === section)
  state.page = m.id
  state.section = hasSection ? section : ''
}

const onKey = (e) => {
  const mod = e.metaKey || e.ctrlKey
  if (mod && e.key === 's') {
    e.preventDefault()
    saveNow()
    return
  }
  if (mod && e.key === 'k') {
    e.preventDefault()
    searchOpen.value = true
    searchInput.value?.focus()
    return
  }
  if (e.key === '/' && !searchOpen.value && !/input|textarea|select/i.test(e.target.tagName)) {
    e.preventDefault()
    searchOpen.value = true
    searchInput.value?.focus()
  }
}

let stopLive = null
onMounted(() => {
  applyTheme(state.theme)
  window.addEventListener('hashchange', applyHash)
  window.addEventListener('keydown', onKey)
  loadAll().then(() => {
    applyHash()
    stopLive = startLive()
  })
})
onUnmounted(() => {
  window.removeEventListener('hashchange', applyHash)
  window.removeEventListener('keydown', onKey)
  stopLive?.()
})

const engineOk = computed(() => state.stats.engine === 'ready')
const devCount = computed(() => state.devices.length)

// 手机底栏：四个高频入口 + 更多
const tabbar = computed(() =>
  modules.value.filter((m) => ['dashboard', 'turns', 'assistant', 'voice'].includes(m.id)),
)
</script>

<template>
  <div class="shell" :class="{ 'nav-open': navOpen }">
    <div v-if="navOpen" class="nav-mask" @click="navOpen = false"></div>

    <aside class="sidebar">
      <div class="brand">
        <Logo :size="30" />
        <div class="grow" style="min-width: 0">
          <div class="brand-name truncate">{{ state.draft?.server?.name || '小爱家庭助手' }}</div>
          <div class="brand-sub">小爱GO</div>
        </div>
      </div>

      <nav class="nav">
        <template v-for="g in visibleGroups" :key="g.id">
          <div class="nav-group">{{ g.label }}</div>
          <template v-for="it in g.items" :key="it.id">
            <a
              class="nav-item"
              :class="{ active: state.page === it.id }"
              :href="'#' + it.id"
              @click.prevent="nav(it.id)"
            >
              <span class="nav-ico"><Ico :name="it.icon" /></span>
              <span class="grow truncate">{{ it.label }}</span>
              <span v-if="it.id === 'turns'" class="nav-badge">{{ state.turns.length }}</span>
              <span v-else-if="it.id === 'assistant'" class="nav-badge">{{
                state.draft?.profiles?.length || 0
              }}</span>
            </a>
            <!-- 当前页的分区，像目录一样展开：一眼看到这页里有什么 -->
            <div v-if="state.page === it.id && it.sections?.length" class="nav-sub">
              <a
                v-for="s in it.sections"
                :key="s.id"
                class="nav-sub-item"
                :class="{ active: state.section === s.id }"
                :href="'#' + it.id + '/' + s.id"
                @click.prevent="nav(it.id, s.id)"
              >
                {{ s.label }}
              </a>
            </div>
          </template>
        </template>
      </nav>

      <div class="sidebar-foot">
        <div class="foot-line">
          <span class="dot" :class="state.streamOk ? 'on' : 'err'"></span>
          <span>{{ state.streamOk ? '实时连接' : '连接断开，重试中' }}</span>
        </div>
        <div class="foot-line">
          <span class="dot" :class="engineOk ? 'on' : 'err'"></span>
          <span>{{
            state.stats.engine === 'ready'
              ? '引擎就绪'
              : state.stats.engine === 'disabled'
                ? '引擎已禁用'
                : '引擎未就绪'
          }}</span>
        </div>
        <div class="foot-line">
          <span class="dot" :class="devCount ? 'on' : ''"></span>
          <span>{{ devCount }} 台设备在线</span>
        </div>
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <button class="btn ghost icon nav-toggle" @click="navOpen = true" aria-label="打开菜单">
          <svg width="18" height="18" viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round">
            <path d="M3 5h12M3 9h12M3 13h12" />
          </svg>
        </button>

        <div class="topbar-head">
          <div class="topbar-title">{{ pageTitle }}</div>
          <div class="topbar-sub">{{ pageSub }}</div>
        </div>
        <!-- 手机上侧栏是抽屉，所以给一个紧凑的分区下拉；桌面端隐藏，靠侧栏目录 -->
        <select
          v-if="cur.sections?.length"
          class="sectselect"
          :value="activeSectionId"
          @change="go(cur.id, $event.target.value)"
        >
          <option v-for="s in cur.sections" :key="s.id" :value="s.id">{{ s.label }}</option>
        </select>

        <!-- 设置搜索：按"我要改什么"找，不按"它在哪个页面"找 -->
        <div class="search" :class="{ open: searchOpen }">
          <span class="search-ico"><Ico name="search" :size="14" /></span>
          <input
            ref="searchInput"
            v-model="state.query"
            class="search-input"
            placeholder="搜索设置…"
            @focus="searchOpen = true"
            @blur="searchOpen = false"
            @keydown="onSearchKey"
          />
          <kbd v-if="!searchOpen" class="search-kbd">/</kbd>
          <div v-if="searchOpen && state.query.trim()" class="search-pop">
            <button
              v-for="(t, i) in results"
              :key="t.page + '/' + t.section"
              class="search-item"
              :class="{ active: i === activeIdx }"
              @mousedown.prevent="nav(t.page, t.section)"
              @mouseenter="activeIdx = i"
            >
              <span class="nav-ico"><Ico :name="t.icon" :size="14" /></span>
              <span class="grow">
                <span class="search-item-label">{{ t.label }}</span>
                <span v-if="t.sub" class="search-item-sub">{{ t.sub }}</span>
              </span>
            </button>
            <div v-if="!results.length" class="search-empty">没有匹配的设置</div>
          </div>
        </div>

        <div class="topbar-right">
          <!-- 保存状态：改完即存，这里只负责"告诉你结果" -->
          <button
            class="savepill"
            :class="saveTone"
            :title="state.saveState === 'error' ? state.saveError : '⌘S 立即保存'"
            @click="saveNow()"
          >
            <span class="savepill-dot"></span>
            <span class="savepill-text">{{ saveLabel }}</span>
            <span v-if="state.saveState === 'saved' && savedAtText" class="savepill-time mono">{{
              savedAtText
            }}</span>
            <span v-else-if="state.saveState === 'error'" class="savepill-retry">重试</span>
          </button>

          <div class="themebtn" :title="'主题：' + themeLabel">
            <button
              v-for="t in themes"
              :key="t.id"
              class="themebtn-item"
              :class="{ on: state.theme === t.id }"
              :aria-label="t.label"
              @click="applyTheme(t.id)"
            >
              <Ico :name="t.icon" :size="14" />
            </button>
          </div>
        </div>
      </header>

      <div class="content">
        <div v-if="state.loading" class="page">
          <div class="skeleton" style="height: 96px"></div>
          <div class="skeleton" style="height: 220px"></div>
        </div>
        <div v-else-if="state.loadError" class="page">
          <div class="notice bad">
            <Ico name="close" />
            <div>
              <div class="strong">配置加载失败</div>
              <div class="mt-2">{{ state.loadError }}</div>
              <button class="btn sm mt-10" @click="loadAll()">重试</button>
            </div>
          </div>
        </div>
        <component v-else :is="curComp" v-bind="compProps" :key="state.page" />
      </div>
    </div>

    <!-- 手机底栏：四个高频入口 + 其余在抽屉里 -->
    <nav class="tabbar">
      <a
        v-for="m in tabbar"
        :key="m.id"
        class="tabbar-item"
        :class="{ on: state.page === m.id }"
        :href="'#' + m.id"
        @click.prevent="nav(m.id)"
      >
        <Ico :name="m.icon" :size="18" />
        <span>{{ m.label }}</span>
      </a>
      <button class="tabbar-item" :class="{ on: navOpen }" @click="navOpen = true">
        <Ico name="sliders" :size="18" />
        <span>更多</span>
      </button>
    </nav>

    <div class="toasts">
      <div v-for="t in state.toasts" :key="t.id" class="toast" :class="t.kind">
        <span class="toast-ico">
          <Ico :name="t.kind === 'bad' ? 'close' : t.kind === 'warn' ? 'clock' : 'check'" />
        </span>
        <span>{{ t.text }}</span>
      </div>
    </div>
  </div>
</template>
