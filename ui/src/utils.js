// 展示层的小工具：时间、体积、耗时的可读化。
export function fmtAgo(iso) {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return '—'
  const s = Math.max(1, Math.round((Date.now() - t) / 1000))
  if (s < 60) return `${s} 秒前`
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  return `${Math.floor(s / 86400)} 天前`
}

export function fmtClock(iso) {
  if (!iso) return '--:--'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '--:--'
  const p = (n) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export function fmtDur(sec) {
  if (!sec || sec < 0) return '0 秒'
  if (sec < 60) return `${Math.round(sec)} 秒`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分 ${Math.round(sec % 60)} 秒`
  const h = Math.floor(sec / 3600)
  const m = Math.round((sec % 3600) / 60)
  return `${h} 时 ${m} 分`
}

export function fmtBytes(n) {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${u[i]}`
}

export function fmtMs(ms) {
  if (ms === undefined || ms === null) return '—'
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

// 延迟条宽度：以 8 秒为满格，超出按满格算
export function pct(ms, full = 8000) {
  return Math.min(100, Math.max(1.5, (ms / full) * 100))
}
