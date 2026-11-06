// 模块注册表。
//
// 导航、页面、页内分区都从这张表读——加一个新页面 = 加一条记录，
// 不用改 App.vue、不用改路由、不用改搜索。
//
// 为插件预留的两个口子：
//   1. 模块整体可以在运行时追加（见 mergeRemoteModules），后端 /api/meta
//      返回的 modules 会被合并进来。插件作者只需要：注册一个页面组件、
//      在 meta 里声明 id/label/group/icon/sections。
//   2. 有分区的页面不需要自己写容器：声明 sections 后由 SectionPage 统一渲染
//      （它会处理 tab、深链 #page/section、上一节/下一节）。
//
// sections[].keywords 是给顶栏搜索用的：用户输入"静音"能直接跳到「听与分段」，
// 而不需要知道它在哪一页。
import { defineAsyncComponent } from 'vue'

const load = (fn) => defineAsyncComponent(fn)

export const GROUPS = [
  { id: 'run', label: '运行' },
  { id: 'assistant', label: '助手' },
  { id: 'voice', label: '语音' },
  { id: 'tools', label: '工具' },
  { id: 'system', label: '系统' },
]

// 有分区的页面统一用这个容器，省得每个页面自己实现 tab
export const SectionPage = load(() => import('./pages/SectionPage.vue'))

export const MODULES = [
  {
    id: 'dashboard',
    group: 'run',
    label: '总览',
    icon: 'grid',
    sub: '设备、引擎与最近表现',
    keywords: ['状态', '概览', '设备', '在线', '延迟', '快速验证'],
    component: load(() => import('./pages/PageDashboard.vue')),
  },
  {
    id: 'turns',
    group: 'run',
    label: '对话记录',
    icon: 'chat',
    sub: '每一轮的提问、回答与耗时',
    keywords: ['记录', '历史', '延迟', '瀑布', '日志'],
    component: load(() => import('./pages/PageTurns.vue')),
  },
  {
    id: 'assistant',
    group: 'assistant',
    label: '助手',
    icon: 'users',
    sub: '谁在被唤醒、怎么对话',
    keywords: ['人格', '提示词'],
    component: SectionPage,
    sections: [
      {
        id: 'profiles',
        label: '助手档案',
        desc: '唤醒词 → 模型、提示词、工具范围、专属发声参数',
        keywords: ['档案', '唤醒词', '模型', '提示词', '工具白名单', '音色', '语速', 'llm'],
        component: load(() => import('./pages/PageProfiles.vue')),
      },
      {
        id: 'behavior',
        label: '对话行为',
        desc: '退出词、连续对话时长、工具轮数、上下文记忆',
        keywords: ['退出词', '超时', '轮数', '上下文', '历史', '记忆', '保鲜期'],
        component: load(() => import('./sections/AssistantBehavior.vue')),
      },
    ],
  },
  {
    id: 'voice',
    group: 'voice',
    label: '语音',
    icon: 'wave',
    sub: '听得见、说得出',
    keywords: ['音频', '引擎'],
    component: SectionPage,
    sections: [
      {
        id: 'models',
        label: '引擎与模型',
        desc: '识别与合成用谁、模型放在哪、推理占几个核',
        keywords: ['模型', '路径', '供应商', 'provider', '线程', 'cpu', 'vad', '阈值', 'tts', 'asr'],
        component: load(() => import('./sections/VoiceModels.vue')),
      },
      {
        id: 'wake',
        label: '唤醒',
        desc: '唤醒词、灵敏度、以及没有唤醒模型时的替代方案',
        keywords: ['唤醒', 'kws', '关键词', '灵敏度', '免唤醒', '文本唤醒'],
        component: load(() => import('./sections/VoiceWake.vue')),
      },
      {
        id: 'listen',
        label: '听与分段',
        desc: '从波形里切出一句话：前置缓冲、静音判定、单段上限',
        keywords: ['分段', '静音', '前置', '缓冲', '最短语音', '灵敏度', '吞字'],
        component: load(() => import('./sections/VoiceListen.vue')),
      },
      {
        id: 'speak',
        label: '说与播报',
        desc: '声音来源、语速、切句、提示音与固定应答',
        keywords: ['语速', '音色', '切句', '播报', '提示音', '应答', '发声', 'tts'],
        component: load(() => import('./sections/VoiceSpeak.vue')),
      },
    ],
  },
  {
    id: 'tools',
    group: 'tools',
    label: '工具与设备',
    icon: 'plug',
    sub: 'Home Assistant、搜索、天气、安全护栏',
    keywords: ['ha', 'home assistant', '搜索', '天气', '工具', '护栏', '自定义'],
    component: load(() => import('./pages/PageTools.vue')),
  },
  {
    id: 'system',
    group: 'system',
    label: '系统与服务',
    icon: 'gear',
    sub: '鉴权、记录、录音、维护',
    keywords: ['令牌', '鉴权', '录音', '配置', '导入', '导出', '重载', 'shell'],
    component: load(() => import('./pages/PageSystem.vue')),
  },
]

// 合并后端下发的模块（插件预留）。
// 约束：只接受带 id / label / component 的项；同 id 不覆盖内置模块，
// 避免一个出错的插件把「语音」页面顶掉。
export function mergeRemoteModules(remote) {
  if (!Array.isArray(remote)) return MODULES
  const seen = new Set(MODULES.map((m) => m.id))
  const extra = []
  for (const m of remote) {
    if (!m || !m.id || !m.label || seen.has(m.id)) continue
    seen.add(m.id)
    extra.push({
      id: m.id,
      group: GROUPS.some((g) => g.id === m.group) ? m.group : 'system',
      label: m.label,
      icon: m.icon || 'plug',
      sub: m.sub || '',
      keywords: m.keywords || [],
      // 插件页面用 url 承载（独立构建的 HTML），内置页面用 component
      url: m.url || '',
      external: !!m.url,
      component: m.url ? null : load(() => import(/* @vite-ignore */ m.entry || './pages/PageSystem.vue')),
      sections: Array.isArray(m.sections) ? m.sections : [],
    })
  }
  return [...MODULES, ...extra]
}

// 搜索：把「模块」和「页内分区」拍平成可跳转的目标
export function searchTargets(modules) {
  const out = []
  for (const m of modules) {
    out.push({
      page: m.id,
      section: '',
      label: m.label,
      group: m.group,
      sub: m.sub,
      icon: m.icon,
      keywords: [m.label, m.sub, ...(m.keywords || [])].join(' ').toLowerCase(),
    })
    for (const s of m.sections || []) {
      out.push({
        page: m.id,
        section: s.id,
        label: `${m.label} · ${s.label}`,
        group: m.group,
        sub: s.desc,
        icon: m.icon,
        keywords: [m.label, s.label, s.desc, ...(s.keywords || [])].join(' ').toLowerCase(),
      })
    }
  }
  return out
}

export function findModule(modules, id) {
  return modules.find((m) => m.id === id) || modules[0]
}
