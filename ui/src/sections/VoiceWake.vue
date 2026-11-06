<script setup>
// 语音 · 唤醒
// 三条路径按推荐顺序排：本地 KWS → 识别文本唤醒 → 免唤醒（调试）。
import { computed, ref } from 'vue'
import Ico from '../components/Ico.vue'
import { state, toast, go } from '../store'
import { api } from '../api'

const e = computed(() => state.draft?.engine || {})
const srv = computed(() => state.draft?.server || {})
const syncing = ref(false)

const wakeWords = computed(() => state.kws.words || [])
const devices = computed(() => state.kws.devices || [])

const syncState = computed(() => {
  if (!devices.value.length) return { kind: '', text: '没有在线设备' }
  const bad = devices.value.filter((d) => d.err)
  if (bad.length) return { kind: 'bad', text: `${bad.length} 台同步失败` }
  const stale = devices.value.filter((d) => d.rev !== state.kws.rev)
  if (stale.length) return { kind: 'warn', text: `${stale.length} 台待同步` }
  return { kind: 'ok', text: '设备已是最新' }
})

async function sync() {
  syncing.value = true
  try {
    const r = await api.kwsSync('')
    const list = Object.values(r.devices || {})
    if (!list.length) toast(r.note || '没有在线设备', 'warn')
    else toast(`已同步到 ${list.length} 台设备`, 'ok')
  } catch (err) {
    toast(err.message, 'bad', 5000)
  } finally {
    syncing.value = false
  }
}
</script>

<template>
  <div class="sheet">
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">音箱端关键词唤醒</div>
          <div class="sect-d">
            在音频流里直接找唤醒词，不用等整句识别完——喊完 0.5 秒内就有回应。
          </div>
        </div>
        <label class="switch">
          <input type="checkbox" v-model="e.kws_enabled" />
          <span class="track"></span>
        </label>
      </div>

      <div v-if="e.kws_enabled">
        <div class="rows">
          <div class="row2">
            <div>
              <div class="row2-label">命中加分</div>
              <div class="row2-hint">越大越不容易被噪声误触发。</div>
            </div>
            <div class="row2-ctl">
              <div class="slider-row">
                <input type="range" min="0.5" max="6" step="0.1" v-model.number="e.kws_score" />
                <span class="slider-value">{{ Number(e.kws_score).toFixed(1) }}</span>
              </div>
            </div>
          </div>
          <div class="row2">
            <div>
              <div class="row2-label">触发阈值</div>
              <div class="row2-hint">总分超过它才算唤醒；调高更保守，调低更灵敏。</div>
            </div>
            <div class="row2-ctl">
              <div class="slider-row">
                <input type="range" min="0.05" max="0.9" step="0.01" v-model.number="e.kws_threshold" />
                <span class="slider-value">{{ Number(e.kws_threshold).toFixed(2) }}</span>
              </div>
            </div>
          </div>
        </div>

        <div class="subcard mt-10">
          <div class="subcard-head">
            <div class="grow">
              <div class="card-title">当前唤醒词</div>
              <div class="tiny mute">来自「助手 · 助手档案」里各档案的词表，改词请去那里。</div>
            </div>
            <span class="tag" :class="syncState.kind">{{ syncState.text }}</span>
            <button class="btn sm" :disabled="syncing" @click="sync()">
              <Ico name="refresh" :size="13" /> {{ syncing ? '同步中…' : '同步到设备' }}
            </button>
            <button class="btn sm ghost" @click="go('assistant', 'profiles')">去改唤醒词</button>
          </div>
          <div class="chips">
            <span v-for="w in wakeWords" :key="w" class="chip on">{{ w }}</span>
            <span v-if="!wakeWords.length" class="chip">还没有唤醒词</span>
          </div>
        </div>

        <div class="notice info mt-10">
          <Ico name="info" />
          <div>
            改唤醒词后服务端会重新生成关键词文件，需要重载引擎才生效；同时会把纯词表推到设备的
            <span class="mono">/data/xiaoai/wakewords.txt</span>，不用登机器改文件。
          </div>
        </div>
      </div>
      <div v-else class="notice warn">
        <Ico name="clock" />
        <div>关掉后只能靠后台触发或下面的替代方案唤醒。日常使用建议开着。</div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">识别文本唤醒</div>
          <div class="sect-d">没有 KWS 模型时的替代：先识别成文字，命中唤醒词就进入对话。</div>
        </div>
        <label class="switch">
          <input type="checkbox" v-model="srv.wake_by_asr" />
          <span class="track"></span>
        </label>
      </div>
      <div class="notice" :class="srv.wake_by_asr ? 'warn' : 'info'">
        <Ico name="clock" />
        <div v-if="srv.wake_by_asr">
          已开启。代价是唤醒多一次识别（约 0.2 秒），而且错字也能容错匹配——2 字唤醒词几乎
          "沾一点边就唤醒"（实测「小同学」会命中「小黑」）。有 KWS 时建议关掉。
        </div>
        <div v-else>
          关闭中。KWS 可用时不需要它；关着也能消除「小爱同学」被误认成「小黑」的情况。
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">免唤醒模式（调试）</div>
          <div class="sect-d">听到人说话就直接回答，不判断有没有喊唤醒词。</div>
        </div>
        <label class="switch">
          <input type="checkbox" v-model="srv.free_talk" />
          <span class="track"></span>
        </label>
      </div>
      <div class="notice warn">
        <Ico name="clock" />
        <div>
          打开后<strong>家里任何对话都会被当成指令回答</strong>，包括电视里说的话。
          只适合在没有 KWS 模型时验证链路，日常使用请关掉。
        </div>
      </div>
    </section>
  </div>
</template>
