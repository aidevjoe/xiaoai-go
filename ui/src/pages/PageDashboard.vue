<script setup>
import { computed, ref } from 'vue'
import Ico from '../components/Ico.vue'
import Field from '../components/Field.vue'
import { state, toast, refreshLive, go } from '../store'
import { api } from '../api'
import { fmtClock, fmtDur, fmtMs } from '../utils'

const s = computed(() => state.stats)
const busy = ref('')
const askText = ref('')

const recent = computed(() => state.turns.slice(0, 6))

async function act(name, fn) {
  busy.value = name
  try {
    await fn()
  } catch (e) {
    toast(e.message, 'bad', 5000)
  } finally {
    busy.value = ''
  }
}

function firstDevice() {
  const d = state.devices[0]
  if (!d) {
    toast('没有设备在线', 'warn')
    return null
  }
  return d
}

const tone = () => {
  const d = firstDevice()
  if (!d) return
  act('tone', async () => {
    const r = await api.tone(d.id)
    toast(`提示音已下发 · 往返 ${r.rtt_ms}ms`, 'ok')
  })
}

const ping = () => {
  const d = firstDevice()
  if (!d) return
  act('ping', async () => {
    const r = await api.ping(d.id)
    toast(`设备响应正常 · 往返 ${r.rtt_ms}ms`, 'ok')
  })
}

const wake = () => {
  const d = firstDevice()
  if (!d) return
  act('wake', async () => {
    const kw = state.draft?.profiles?.[0]?.wake_words?.[0] || '小黑'
    const r = await api.wake(d.id, kw)
    toast(`已用「${kw}」唤醒 · 档案 ${r.profile} · 状态 ${r.state}`, 'ok')
  })
}

const ask = () => {
  const d = firstDevice()
  if (!d || !askText.value.trim()) return
  act('ask', async () => {
    const r = await api.ask(d.id, askText.value.trim(), true)
    if (r.ignored) {
      toast('未唤醒，已忽略。用「模拟唤醒」先激活会话', 'warn', 5000)
      return
    }
    const t = r.turn || {}
    toast(`答：${t.reply || '(空)'} · 首字 ${fmtMs(t.first_delta_ms)}`, 'ok', 7000)
    askText.value = ''
  })
}

const reloadEngine = () =>
  act('engine', async () => {
    const r = await api.engineReload()
    toast(`引擎已重载 · ${r.status}`, 'ok', 5000)
    await refreshLive()
  })

// 音量条：把 0-100 映射成 9 根柱子
function bars(level) {
  const n = 9
  const on = Math.round((Math.min(100, Math.max(0, level || 0)) / 100) * n)
  return Array.from({ length: n }, (_, i) => (i < on ? 100 : 16))
}
</script>

<template>
  <div class="page">
    <!-- 关键指标 -->
    <div class="grid-4">
      <div class="metric">
        <div class="metric-label">在线设备</div>
        <div class="metric-value">
          {{ s.devices || 0 }}<small>台</small>
        </div>
        <div class="tiny mute">{{ state.devices[0]?.model || '等待音箱连接' }}</div>
      </div>
      <div class="metric">
        <div class="metric-label">平均首字延迟</div>
        <div class="metric-value">
          {{ s.avg_first_ms || 0 }}<small>ms</small>
        </div>
        <div class="tiny mute">
          最快 {{ s.min_first_ms || 0 }}ms · 最慢 {{ s.max_first_ms || 0 }}ms
        </div>
      </div>
      <div class="metric">
        <div class="metric-label">平均整轮耗时</div>
        <div class="metric-value">
          {{ s.avg_total_ms || 0 }}<small>ms</small>
        </div>
        <div class="tiny mute">{{ s.turns || 0 }} 轮中成功 {{ s.ok_turns || 0 }} 轮</div>
      </div>
      <div class="metric">
        <div class="metric-label">服务内存</div>
        <div class="metric-value">
          {{ s.heap_mb || 0 }}<small>MB</small>
        </div>
        <div class="tiny mute">{{ s.goroutines || 0 }} 协程 · 运行 {{ fmtDur(s.uptime_sec) }}</div>
      </div>
    </div>

    <div class="grid-2">
      <!-- 设备 -->
      <div class="card">
        <div class="card-head">
          <Ico name="plug" />
          <div class="grow">
            <div class="card-title">设备</div>
            <div class="card-desc">长连接上的音箱，实时音量与流量</div>
          </div>
          <button class="btn sm ghost" @click="refreshLive()">
            <Ico name="refresh" :size="13" />
          </button>
        </div>
        <div class="list">
          <div v-for="d in state.devices" :key="d.id" class="row">
            <div class="grow">
              <div class="flex items-center gap-6">
                <span class="dot on"></span>
                <span class="strong">{{ d.model || '未知型号' }}</span>
                <span class="tag">{{ d.firmware || '—' }}</span>
              </div>
              <div class="tiny mute mono mt-2 truncate">
                {{ d.id }} · {{ d.remote }} · 在线 {{ fmtDur(d.online_sec) }}
              </div>
              <div class="wave mt-6">
                <i v-for="(h, i) in bars(d.level)" :key="i" :style="{ height: h + '%' }"></i>
              </div>
            </div>
            <div class="tiny mute mono nowrap" style="text-align: right">
              {{ d.level }}%<br />
              {{ d.frames }} 帧
            </div>
          </div>
          <div v-if="!state.devices.length" class="empty">
            <span class="empty-ico"><Ico name="plug" :size="26" /></span>
            <div>还没有设备连接</div>
            <div class="tiny">
              在音箱上启动 client 并指向
              <span class="mono">{{ state.meta.device_addr }}</span>
            </div>
          </div>
        </div>
        <div class="card-foot">
          <button class="btn sm" :disabled="!state.devices.length || !!busy" @click="ping()">
            <Ico name="refresh" :size="13" /> 读取信息
          </button>
          <button class="btn sm" :disabled="!state.devices.length || !!busy" @click="tone()">
            <Ico name="play" :size="13" /> 播放提示音
          </button>
          <button class="btn sm" :disabled="!state.devices.length || !!busy" @click="wake()">
            <Ico name="wave" :size="13" /> 模拟唤醒
          </button>
        </div>
      </div>

      <!-- 引擎 -->
      <div class="card">
        <div class="card-head">
          <Ico name="cpu" />
          <div class="grow">
            <div class="card-title">语音引擎</div>
            <div class="card-desc">本地识别、合成与唤醒词</div>
          </div>
          <span
            class="tag"
            :class="s.engine === 'ready' ? 'ok' : s.engine === 'disabled' ? '' : 'bad'"
          >
            {{ s.engine === 'ready' ? '就绪' : s.engine === 'disabled' ? '已禁用' : '未就绪' }}
          </span>
        </div>
        <div class="card-body">
          <Field label="加载详情">
            <div class="mono small dim" style="word-break: break-all">
              {{ s.engine_status || (s.engine || '').replace('error: ', '') || '—' }}
            </div>
          </Field>
          <Field label="模型目录">
            <div class="mono small dim truncate">{{ state.meta.models || '—' }}</div>
          </Field>
          <div class="grid-2">
            <div class="metric">
              <div class="metric-label">上行流量</div>
              <div class="metric-value" style="font-size: 15px">
                {{ (s.audio_kbps || 0).toFixed(1) }}<small>kbps</small>
              </div>
            </div>
            <div class="metric">
              <div class="metric-label">归档对话</div>
              <div class="metric-value" style="font-size: 15px">
                {{ s.turns || 0 }}<small>轮</small>
              </div>
            </div>
          </div>
        </div>
        <div class="card-foot">
          <button
            class="btn sm"
            :disabled="state.meta.no_engine || busy === 'engine'"
            @click="reloadEngine()"
          >
            <Ico name="refresh" :size="13" />
            {{ busy === 'engine' ? '重载中…' : '重载引擎' }}
          </button>
          <span class="tiny mute">改了模型路径或阈值后需要重载</span>
        </div>
      </div>
    </div>

    <!-- 快速验证 -->
    <div class="card">
      <div class="card-head">
        <Ico name="chat" />
        <div class="grow">
          <div class="card-title">快速验证</div>
          <div class="card-desc">跳过录音，直接把一句话送进对话链路，验证提示词与工具</div>
        </div>
      </div>
      <div class="card-body">
        <div class="input-group">
          <input
            v-model="askText"
            class="input"
            placeholder="例如：客厅灯开着吗 / 现在几点"
            @keyup.enter="ask()"
          />
          <button
            class="btn primary"
            :disabled="!askText.trim() || !state.devices.length || busy === 'ask'"
            @click="ask()"
          >
            {{ busy === 'ask' ? '思考中…' : '发送' }}
          </button>
        </div>
        <div class="notice info">
          <Ico name="clock" />
          <div>
            这条请求会绕过唤醒限制（方便调试），但回复仍会真实播报到音箱上。没有设备时先在音箱上启动 client。
          </div>
        </div>
      </div>
    </div>

    <!-- 实时对话 -->
    <div class="card">
      <div class="card-head">
        <Ico name="wave" />
        <div class="grow">
          <div class="card-title">最近对话</div>
          <div class="card-desc">最新的在最上面，实时更新</div>
        </div>
        <button class="btn sm ghost" @click="go('turns')">查看全部</button>
      </div>
      <div class="list">
        <div v-for="(t, i) in recent" :key="i" class="turn">
          <div class="turn-time">{{ fmtClock(t.at) }}</div>
          <div class="turn-main">
            <div class="turn-q">{{ t.text }}</div>
            <div class="turn-a">{{ t.reply || '(无回复)' }}</div>
            <div class="tiny mute mono mt-2">
              {{ t.profile_id }} · 工具 {{ t.tool_rounds }} 次 · {{ t.sentences }} 句
            </div>
          </div>
          <div class="turn-meta">
            <span class="tag ok mono">出声 {{ fmtMs(t.first_audio_ms) }}</span>
            <span class="tag mono">{{ fmtMs(t.total_ms) }}</span>
          </div>
        </div>
        <div v-if="!recent.length" class="empty">
          <span class="empty-ico"><Ico name="chat" :size="26" /></span>
          <div>还没有对话记录</div>
          <div class="tiny">对音箱说一句「小黑小黑」试试</div>
        </div>
      </div>
    </div>
  </div>
</template>
