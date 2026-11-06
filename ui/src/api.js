// 与后端管理面通信的最小封装。
// 统一处理错误信息：后端把原因写在 {error} 里，直接抛出去给 toast 显示。

// 请求超时：fetch 默认会一直等下去，服务端一旦卡住，
// 顶栏保存状态就会永远停在「保存中…」，用户还以为已经存好了。
// 合成/识别可能跑十几秒，所以 raw 请求给更长的窗口。
const DEFAULT_TIMEOUT = 15000
const RAW_TIMEOUT = 60000

async function req(method, path, body, opts = {}) {
  const { timeout, ...fetchOpts } = opts
  const init = { method, headers: {}, ...fetchOpts }
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  const ctl = new AbortController()
  init.signal = ctl.signal
  const ms = timeout || (opts.raw ? RAW_TIMEOUT : DEFAULT_TIMEOUT)
  const timer = setTimeout(() => ctl.abort(), ms)
  let res
  try {
    res = await fetch(path, init)
  } catch (e) {
    if (e.name === 'AbortError') {
      throw new Error(`请求超时（${Math.round(ms / 1000)} 秒），服务端可能正忙`)
    }
    throw e
  } finally {
    clearTimeout(timer)
  }
  const ct = res.headers.get('content-type') || ''
  if (opts.raw) return res
  if (!ct.includes('application/json')) {
    const text = await res.text()
    if (!res.ok) throw new Error(text || `HTTP ${res.status}`)
    return text
  }
  const data = await res.json()
  if (!res.ok) throw new Error(data.error || data.msg || `HTTP ${res.status}`)
  return data
}

export const api = {
  get: (p) => req('GET', p),
  post: (p, b) => req('POST', p, b),
  put: (p, b) => req('PUT', p, b),
  del: (p) => req('DELETE', p),

  health: () => req('GET', '/api/health'),
  stats: () => req('GET', '/api/stats'),
  meta: () => req('GET', '/api/meta'),

  devices: () => req('GET', '/api/devices'),
  tone: (id, body) => req('POST', `/api/devices/${encodeURIComponent(id)}/tone`, body || {}),
  ping: (id) => req('POST', `/api/devices/${encodeURIComponent(id)}/ping`),
  shell: (id, script) => req('POST', `/api/devices/${encodeURIComponent(id)}/shell`, { script }),
  recStart: (id) => req('POST', `/api/devices/${encodeURIComponent(id)}/record/start`),
  recStop: (id) => req('POST', `/api/devices/${encodeURIComponent(id)}/record/stop`),

  settings: () => req('GET', '/api/settings'),
  saveSettings: (s) => req('PUT', '/api/settings', s),
  turns: (limit) => req('GET', `/api/turns?limit=${limit || 80}`),
  clearTurns: () => req('DELETE', '/api/turns'),

  engineReload: () => req('POST', '/api/engine/reload'),
  engineTTS: (text, opts) => req('POST', '/api/engine/tts', { text, ...(opts || {}) }, { raw: true }),
  engineASR: (buf) =>
    req('POST', '/api/engine/asr', undefined, {
      raw: true,
      headers: { 'Content-Type': 'application/octet-stream' },
      body: buf,
    }),

  // 多供应商 TTS
  ttsProviders: () => req('GET', '/api/tts/providers'),
  // 拉某供应商的音色清单（在线清单 + 复刻音色）。参数取服务端已保存的配置。
  // refresh=1 绕过服务端缓存重新拉（「刷新音色」按钮）
  ttsVoices: (provider, refresh) =>
    req(
      'GET',
      `/api/tts/voices?provider=${encodeURIComponent(provider)}${refresh ? '&refresh=1' : ''}`,
    ),
  // 试听：provider/voice 留空就用全局配置
  ttsPreview: (body) => req('POST', '/api/tts/preview', body, { raw: true, timeout: 45000 }),
  ttsClone: (body) => req('POST', '/api/tts/clone', body, { timeout: 90000 }),
  ttsCloneStatus: (provider, speakerId) =>
    req(
      'GET',
      `/api/tts/clone?provider=${encodeURIComponent(provider || '')}&speaker_id=${encodeURIComponent(speakerId)}`,
    ),

  toolsTest: () => req('POST', '/api/tools/test'),
  toolsTestSearch: (query) => req('POST', '/api/tools/test-search', { query: query || '' }),
  toolsTestWeather: () => req('POST', '/api/tools/test-weather'),

  kws: () => req('GET', '/api/kws'),
  kwsSync: (device) => req('POST', '/api/kws/sync', { device: device || '' }),
  models: (dir) => req('GET', `/api/models${dir ? `?dir=${encodeURIComponent(dir)}` : ''}`),
  recordings: () => req('GET', '/api/recordings'),

  wake: (device, keyword) => req('POST', '/api/debug/wake', { device, keyword }),
  ask: (device, text, force) =>
    req('POST', `/api/debug/ask${force ? '?force=1' : ''}`, { device, text }),
}

// SSE：对话与状态实时推送。断了自动重连。
export function connectStream(onEvent) {
  let es = null
  let closed = false
  let retry = null

  const open = () => {
    if (closed) return
    es = new EventSource('/api/stream')
    es.onmessage = (e) => {
      try {
        onEvent(JSON.parse(e.data))
      } catch {
        /* 忽略无法解析的帧 */
      }
    }
    es.onerror = () => {
      es?.close()
      es = null
      onEvent({ type: 'stream', data: { ok: false } })
      retry = setTimeout(open, 2500)
    }
    es.onopen = () => onEvent({ type: 'stream', data: { ok: true } })
  }
  open()

  return () => {
    closed = true
    clearTimeout(retry)
    es?.close()
  }
}
