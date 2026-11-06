<script setup>
// 语音 · 说与播报
// 按「一句话怎么变成声音」的顺序排：来源 → 语速 → 切句 → 缓冲 → 提示音 → 固定应答。
import { computed, ref, onUnmounted } from 'vue'
import Ico from '../components/Ico.vue'
import { state, toast, saveNow, refreshLive } from '../store'
import { api } from '../api'
import { fmtBytes } from '../utils'

const v = computed(() => state.draft?.voice || {})

// 从设备 TTS 切回本地时，若「引擎与模型」里把本地合成关成了 none，
// 要顺手恢复供应商并重载引擎：光改配置的话，已加载的引擎里仍然没有 TTS，
// 逐句合成会全部失败、整轮没声音。
async function setBackend(next) {
  const eng = state.draft?.engine
  const needReload = next === 'local' && eng?.tts_provider === 'none'
  if (needReload) eng.tts_provider = ''
  v.value.backend = next
  if (!needReload) return
  try {
    await saveNow()
    const r = await api.engineReload()
    toast(`已恢复本地合成（引擎重载 ${r.ms}ms）`, 'ok', 5000)
    await refreshLive()
  } catch (err) {
    toast('已切回本地，但引擎重载失败，请到「引擎与模型」点一次重载：' + err.message, 'bad', 7000)
  }
}

const sample = ref('好的，客厅的灯已经打开了。还需要我把温度调到 26 度吗？今晚可能会有点冷。')
const parts = computed(() => {
  const max = Math.max(4, Number(v.value.max_sentence_runes) || 20)
  const text = v.value.strip_emoji ? sample.value.replace(/[*#`~_|]/g, '') : sample.value
  const out = []
  let cur = ''
  const ends = '。！？；…!?;\n'
  for (const ch of text) {
    cur += ch
    if (ends.includes(ch)) {
      out.push(cur.trim())
      cur = ''
    } else if ([...cur].length >= max) {
      out.push(cur.trim())
      cur = ''
    }
  }
  if (cur.trim()) out.push(cur.trim())
  return out
})

const ttsText = ref('客厅的灯已经打开了，还需要我做点什么吗？')
const ttsUrl = ref('')
const ttsInfo = ref('')
const ttsErr = ref('')
const busy = ref('')

async function tryTTS() {
  if (!ttsText.value.trim()) return
  busy.value = 'tts'
  ttsErr.value = ''
  const t0 = performance.now()
  try {
    const res = await api.engineTTS(ttsText.value.trim())
    if (!res.ok) {
      const data = await res.json().catch(() => ({}))
      throw new Error(data.error || `HTTP ${res.status}`)
    }
    const blob = await res.blob()
    if (ttsUrl.value) URL.revokeObjectURL(ttsUrl.value)
    ttsUrl.value = URL.createObjectURL(blob)
    ttsInfo.value = `${fmtBytes(blob.size)} · 合成 ${Math.round(performance.now() - t0)}ms`
  } catch (err) {
    ttsErr.value = err.message
  } finally {
    busy.value = ''
  }
}
onUnmounted(() => {
  if (ttsUrl.value) URL.revokeObjectURL(ttsUrl.value)
})
</script>

<template>
  <div class="sheet">
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">声音来源</div>
          <div class="sect-d">本地合成不依赖网络、语速可调；音箱原生更自然但不支持调速。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">用哪种声音</div>
            <div class="row2-hint">切换后立刻生效，不需要重载模型。</div>
          </div>
          <div class="row2-ctl">
            <div class="seg seg-block" style="max-width: 340px">
              <button :class="{ on: v.backend !== 'device' }" @click="setBackend('local')">本地合成</button>
              <button :class="{ on: v.backend === 'device' }" @click="setBackend('device')">音箱原生 TTS</button>
            </div>
          </div>
        </div>
      </div>
      <div class="notice" :class="v.backend === 'device' ? 'info' : 'ok'">
        <Ico name="wave" />
        <div v-if="v.backend === 'device'">
          <strong>音箱原生 TTS</strong>：走设备自己的云端合成（<span class="mono">tts_play.sh</span>），
          吐字清楚、语气自然。代价：语速由设备决定、依赖网络，且是整句下发，首句要等云端合成。
        </div>
        <div v-else>
          <strong>本地合成</strong>：不依赖网络、语速可调、可以流式逐句合成。音色与采样率由模型决定，
          见「引擎与模型」页头。
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">语速与切句</div>
          <div class="sect-d">语速只影响听感；想更快听到回答，该调的是单句长度。</div>
        </div>
        <span class="tag mono">{{ Number(v.speed || 1).toFixed(2) }}×</span>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">语速</div>
            <div class="row2-hint">合成时直接调参，不重建模型。单个助手也可在档案里覆盖。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <span class="tiny mute">慢</span>
              <input type="range" min="0.5" max="2" step="0.05" v-model.number="v.speed" />
              <span class="tiny mute">快</span>
              <span class="slider-value">{{ Number(v.speed || 1).toFixed(2) }}×</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">单句长度上限</div>
            <div class="row2-hint">越短首字越快，语气越碎；实测 18~24 字比较自然。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="8" max="60" step="2" v-model.number="v.max_sentence_runes" />
              <span class="slider-value">{{ v.max_sentence_runes }} 字</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">起播预缓冲</div>
            <div class="row2-hint">攒够这么多音频再开始播，避免卡顿；太大则延迟明显。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="40" max="400" step="20" v-model.number="v.prebuffer_ms" />
              <span class="slider-value">{{ v.prebuffer_ms }}ms</span>
            </div>
          </div>
        </div>
      </div>

      <div class="subcard mt-10">
        <div class="card-title">切句预览</div>
        <div class="flex gap-6 wrap">
          <span v-for="(p, i) in parts" :key="i" class="tag" :class="i === 0 ? 'ok' : ''">
            {{ i + 1 }}. {{ p }}
          </span>
        </div>
        <div class="tiny mute">第一句一完整就送去合成，所以首字延迟 ≈ 第一句的合成时间 + 一次网络往返。</div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">播报提示</div>
          <div class="sect-d">收到话、说完了、出错各有各的声音；文本在合成前先做清洗。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">应答提示音</div>
            <div class="row2-hint">收到话 / 说完了 / 出错，三种状态各有各的声音。</div>
          </div>
          <div class="row2-ctl">
            <label class="switch"><input type="checkbox" v-model="v.ack_tone" /><span class="track"></span></label>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">过滤表情与 markdown</div>
            <div class="row2-hint">否则 TTS 会念出「星号星号」或者直接合成失败。</div>
          </div>
          <div class="row2-ctl">
            <label class="switch"><input type="checkbox" v-model="v.strip_emoji" /><span class="track"></span></label>
          </div>
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">固定应答</div>
          <div class="sect-d">不经过模型，直接合成播报；同一句第二次播放走缓存。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">唤醒应答</div>
            <div class="row2-hint">留空则只播提示音。</div>
          </div>
          <div class="row2-ctl"><input v-model="v.wake_reply" class="input" placeholder="我在" /></div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">退出应答</div>
            <div class="row2-hint">说退出词时播这一句。</div>
          </div>
          <div class="row2-ctl"><input v-model="v.exit_reply" class="input" placeholder="好，有事再叫我" /></div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">出错应答</div>
            <div class="row2-hint">模型报错时给个反馈，别让用户面对沉默。</div>
          </div>
          <div class="row2-ctl"><input v-model="v.error_reply" class="input" placeholder="抱歉，出了点问题" /></div>
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">试听</div>
          <div class="sect-d">改完上面的参数，用耳朵验证一遍。</div>
        </div>
      </div>
      <div class="subcard">
        <textarea v-model="ttsText" class="textarea" rows="2"></textarea>
        <div class="flex items-center gap-8 wrap">
          <button class="btn primary sm" :disabled="busy === 'tts'" @click="tryTTS()">
            <Ico name="play" :size="13" /> {{ busy === 'tts' ? '合成中…' : '合成并播放' }}
          </button>
          <span class="tiny mute">{{ ttsInfo }}</span>
        </div>
        <audio v-if="ttsUrl" :src="ttsUrl" controls style="width: 100%; height: 34px"></audio>
        <div v-if="ttsErr" class="notice bad"><Ico name="close" /><span class="mono small">{{ ttsErr }}</span></div>
      </div>
    </section>
  </div>
</template>
