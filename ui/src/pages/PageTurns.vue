<script setup>
import { computed, ref } from 'vue'
import Ico from '../components/Ico.vue'
import { state, toast, refreshLive } from '../store'
import { api } from '../api'
import { fmtClock, fmtMs, pct } from '../utils'

const q = ref('')
const only = ref('all') // all | ok | err
const detail = ref(null)
const loading = ref(false)

const rows = computed(() => {
  const kw = q.value.trim().toLowerCase()
  return state.turns.filter((t) => {
    if (only.value === 'ok' && t.err) return false
    if (only.value === 'err' && !t.err) return false
    if (!kw) return true
    return (
      (t.text || '').toLowerCase().includes(kw) ||
      (t.reply || '').toLowerCase().includes(kw) ||
      (t.profile_id || '').toLowerCase().includes(kw) ||
      (t.device || '').toLowerCase().includes(kw)
    )
  })
})

const stats = computed(() => {
  const list = rows.value
  const ok = list.filter((t) => !t.err)
  const sum = ok.reduce((a, t) => a + (t.first_delta_ms || 0), 0)
  const tot = ok.reduce((a, t) => a + (t.total_ms || 0), 0)
  return {
    n: list.length,
    ok: ok.length,
    err: list.length - ok.length,
    avgFirst: ok.length ? Math.round(sum / ok.length) : 0,
    avgTotal: ok.length ? Math.round(tot / ok.length) : 0,
  }
})

// 单轮的工具耗时瀑布：把 round_ms 逐轮画出来
const rounds = computed(() => {
  const t = detail.value
  if (!t || !t.round_ms) return []
  return t.round_ms.map((ms, i) => ({ i: i + 1, ms }))
})

const maxRound = computed(() => Math.max(1, ...rounds.value.map((r) => r.ms)))

async function reload() {
  loading.value = true
  try {
    await refreshLive()
  } finally {
    loading.value = false
  }
}

async function clearAll() {
  try {
    await api.clearTurns()
    state.turns = []
    toast('对话记录已清空', 'ok')
  } catch (e) {
    toast(e.message, 'bad')
  }
}

function copy(text) {
  navigator.clipboard?.writeText(text).then(
    () => toast('已复制', 'ok', 1600),
    () => toast('复制失败', 'bad'),
  )
}
</script>

<template>
  <div class="page">
    <div class="card">
      <div class="card-head">
        <Ico name="chat" />
        <div class="grow">
          <div class="card-title">对话记录</div>
          <div class="card-desc">
            共 {{ stats.n }} 轮 · 成功 {{ stats.ok }} · 失败 {{ stats.err }} · 平均首字
            {{ stats.avgFirst }}ms · 平均整轮 {{ stats.avgTotal }}ms
          </div>
        </div>
        <button class="btn sm ghost" :disabled="loading" @click="reload()">
          <Ico name="refresh" :size="13" /> 刷新
        </button>
        <button class="btn sm danger" :disabled="!state.turns.length" @click="clearAll()">
          <Ico name="trash" :size="13" /> 清空
        </button>
      </div>

      <div class="card-body" style="padding-bottom: 0">
        <div class="input-group">
          <input v-model="q" class="input" placeholder="搜索提问、回答、档案或设备 id…" />
          <div class="seg">
            <button :class="{ on: only === 'all' }" @click="only = 'all'">全部</button>
            <button :class="{ on: only === 'ok' }" @click="only = 'ok'">成功</button>
            <button :class="{ on: only === 'err' }" @click="only = 'err'">失败</button>
          </div>
        </div>
      </div>

      <div class="list">
        <div
          v-for="(t, i) in rows"
          :key="i"
          class="turn"
          style="cursor: pointer"
          @click="detail = t"
        >
          <div class="turn-time">{{ fmtClock(t.at) }}</div>
          <div class="turn-main">
            <div class="turn-q">{{ t.text }}</div>
            <div v-if="t.pending" class="turn-a" style="opacity: 0.55">处理中…</div>
            <div v-else class="turn-a">{{ t.reply || '(无回复)' }}</div>
            <div v-if="t.err" class="turn-err">{{ t.err }}</div>
            <div class="tiny mute mono mt-2">
              <span v-if="t.device">{{ t.device }} · </span>{{ t.profile_id }} · 工具 {{ t.tool_rounds }} 次 ·
              {{ t.sentences }} 句
            </div>
          </div>
          <div class="turn-meta">
            <span v-if="t.pending" class="tag">处理中</span>
            <span v-else-if="t.err" class="tag bad">失败</span>
            <template v-else>
              <span class="tag ok mono">出声 {{ fmtMs(t.first_audio_ms) }}</span>
              <span class="tag info mono">首字 {{ fmtMs(t.first_delta_ms) }}</span>
              <span class="tag mono">{{ fmtMs(t.total_ms) }}</span>
            </template>
          </div>
        </div>
        <div v-if="!rows.length" class="empty">
          <span class="empty-ico"><Ico name="chat" :size="26" /></span>
          <div>{{ state.turns.length ? '没有匹配的记录' : '还没有对话记录' }}</div>
        </div>
      </div>
    </div>

    <!-- 详情抽屉 -->
    <template v-if="detail">
      <div class="drawer-mask" @click="detail = null"></div>
      <aside class="drawer">
        <div class="drawer-head">
          <div class="grow">
            <div class="card-title">这一轮发生了什么</div>
            <div class="tiny mute mono">{{ fmtClock(detail.at) }} · {{ detail.profile_id }}</div>
          </div>
          <button class="btn sm ghost" @click="detail = null">
            <Ico name="close" :size="13" />
          </button>
        </div>

        <div class="drawer-body">
          <div class="notice" :class="detail.err ? 'bad' : 'ok'">
            <Ico :name="detail.err ? 'close' : 'check'" />
            <div>
              <div class="strong">{{ detail.err ? '本轮失败' : '本轮成功' }}</div>
              <div v-if="detail.err" class="mono small mt-2">{{ detail.err }}</div>
              <div v-else class="small mt-2">
                首字 {{ fmtMs(detail.first_delta_ms) }} · 整轮 {{ fmtMs(detail.total_ms) }} ·
                {{ detail.tool_rounds }} 次工具调用（模型调用 {{ detail.rounds }} 次） · 播报 {{ detail.sentences }} 句
              </div>
            </div>
          </div>

          <div class="card">
            <div class="card-head">
              <div class="card-title">提问</div>
              <div class="grow"></div>
              <button class="btn sm ghost" @click="copy(detail.text)">
                <Ico name="copy" :size="12" />
              </button>
            </div>
            <div class="card-body" style="gap: 0">
              <div class="small">{{ detail.text }}</div>
              <div v-if="detail.wake_word" class="tiny mute mt-6">
                唤醒词：{{ detail.wake_word }}
              </div>
            </div>
          </div>

          <div class="card">
            <div class="card-head">
              <div class="card-title">回复</div>
              <div class="grow"></div>
              <button class="btn sm ghost" @click="copy(detail.reply || '')">
                <Ico name="copy" :size="12" />
              </button>
            </div>
            <div class="card-body" style="gap: 0">
              <div class="small">{{ detail.reply || '(空)' }}</div>
            </div>
          </div>

          <div class="card">
            <div class="card-head">
              <div class="card-title">延迟分解</div>
              <div class="card-desc">哪一段慢，一眼看出来</div>
            </div>
            <div class="card-body">
              <div class="wf">
                <div class="wf-row">
                  <span class="wf-label">首字</span>
                  <div class="wf-track">
                    <div
                      class="wf-fill"
                      :style="{ width: pct(detail.first_delta_ms, detail.total_ms || 8000) + '%' }"
                    ></div>
                  </div>
                  <span class="wf-val">{{ fmtMs(detail.first_delta_ms) }}</span>
                </div>
                <div class="wf-row">
                  <span class="wf-label">出声</span>
                  <div class="wf-track">
                    <div
                      class="wf-fill"
                      :style="{
                        width: pct(detail.first_audio_ms, detail.total_ms || 8000) + '%',
                        background: 'linear-gradient(90deg,#3ddc97,#46b6f5)',
                      }"
                    ></div>
                  </div>
                  <span class="wf-val">{{ fmtMs(detail.first_audio_ms) }}</span>
                </div>
                <div class="wf-row">
                  <span class="wf-label">整轮</span>
                  <div class="wf-track">
                    <div class="wf-fill" style="width: 100%"></div>
                  </div>
                  <span class="wf-val">{{ fmtMs(detail.total_ms) }}</span>
                </div>
                <div v-for="r in rounds" :key="r.i" class="wf-row">
                  <span class="wf-label">第 {{ r.i }} 次调用</span>
                  <div class="wf-track">
                    <div
                      class="wf-fill"
                      :style="{
                        width: (r.ms / maxRound) * 100 + '%',
                        background: 'linear-gradient(90deg,#a78bfa,#46b6f5)',
                      }"
                    ></div>
                  </div>
                  <span class="wf-val">{{ fmtMs(r.ms) }}</span>
                </div>
              </div>
              <div class="notice info mt-10">
                <Ico name="clock" />
                <div>
                  模型调用次数越多首字越慢：每多一次「工具→再问」就是一次完整的往返。能预取的信息（时间、天气）已经在提示词里，不该再做成工具。
                </div>
              </div>
            </div>
          </div>
        </div>

        <div class="drawer-foot">
          <button class="btn" @click="detail = null">关闭</button>
        </div>
      </aside>
    </template>
  </div>
</template>
