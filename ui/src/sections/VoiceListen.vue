<script setup>
// 语音 · 听与分段
// 时间轴把三个阈值画出来，改一个数字就能看到比例变化。
import { computed, ref } from 'vue'
import Ico from '../components/Ico.vue'
import { state, toast, revertSection } from '../store'
import { api } from '../api'
import { fmtBytes } from '../utils'

const d = computed(() => state.draft?.dialog || {})
const asrTag = computed(
  () => state.stats.engine_status?.match(/ASR=([^\s]+)/)?.[1] || '未就绪',
)

const timeline = computed(() => {
  const pre = Number(d.value.pre_roll_ms) || 200
  const speech = Number(d.value.min_speech_ms) || 250
  const silence = Number(d.value.silence_ms) || 300
  const total = pre + speech + silence || 1
  return {
    pre: (pre / total) * 100,
    speech: (speech / total) * 100,
    silence: (silence / total) * 100,
    total,
  }
})

const busy = ref('')
const asrFile = ref(null)
const asrResult = ref('')
const asrInfo = ref('')

async function tryASR(ev) {
  const f = ev.target.files?.[0]
  if (!f) return
  asrFile.value = f
  busy.value = 'asr'
  asrResult.value = ''
  try {
    let buf = await f.arrayBuffer()
    const head = new Uint8Array(buf.slice(0, 4))
    if (String.fromCharCode(...head) === 'RIFF') buf = buf.slice(44)
    const res = await api.engineASR(buf)
    const data = await res.json()
    if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`)
    asrResult.value = data.text || '(空)'
    asrInfo.value = `音频 ${data.pcm_ms}ms · 识别 ${data.ms}ms · RTF ${(
      data.ms / Math.max(1, data.pcm_ms)
    ).toFixed(2)}`
  } catch (err) {
    toast('识别失败：' + err.message, 'bad', 6000)
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <div class="sheet">
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">一句话的边界</div>
          <div class="sect-d">
            从连续波形里切出「一句话」交给识别模型。调得太急会吞字，调得太慢会等半天。
          </div>
        </div>
        <button class="btn sm ghost" @click="revertSection('dialog')">恢复上次保存</button>
      </div>

      <div class="timeline mt-10">
        <div class="timeline-bar">
          <div class="timeline-seg pre" :style="{ width: timeline.pre + '%' }">前置</div>
          <div class="timeline-seg speech" :style="{ width: timeline.speech + '%' }">确认语音</div>
          <div class="timeline-seg silence" :style="{ width: timeline.silence + '%' }">静音收尾</div>
        </div>
        <div class="tiny mute mt-6">
          合计 {{ timeline.total }}ms 才会把一段话交出去；说话期间最多录 {{ d.max_utterance_ms }}ms
        </div>
      </div>

      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">前置缓冲</div>
            <div class="row2-hint">确认语音之前的声音也留住，否则开头第一个字会被切掉；500ms 以下容易吞字。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="60" max="900" step="20" v-model.number="d.pre_roll_ms" />
              <span class="slider-value">{{ d.pre_roll_ms }}ms</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">最短语音</div>
            <div class="row2-hint">连续说这么久才算真的在说话，过滤咳嗽、关门声。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="100" max="800" step="25" v-model.number="d.min_speech_ms" />
              <span class="slider-value">{{ d.min_speech_ms }}ms</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">静音判定</div>
            <div class="row2-hint">停顿超过它就认为说完了。太小会把一句话切成两半。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="150" max="1200" step="25" v-model.number="d.silence_ms" />
              <span class="slider-value">{{ d.silence_ms }}ms</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">单段上限</div>
            <div class="row2-hint">一直说话时的强制切断点，防止把内存吃光。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="5000" max="30000" step="1000" v-model.number="d.max_utterance_ms" />
              <span class="slider-value">{{ d.max_utterance_ms / 1000 }}s</span>
            </div>
          </div>
        </div>
      </div>

      <div class="notice warn mt-10">
        <Ico name="clock" />
        <div>
          改这里会重建每台设备的音频管道（正在说话的设备会重置一次），但不需要重载模型，保存后立刻可用。
          VAD 的人声阈值在「引擎与模型」里，两处配合决定灵敏度。
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">识别自测</div>
          <div class="sect-d">上传一段 16k 单声道 WAV，用当前模型跑一遍，看文本与 RTF。</div>
        </div>
        <span class="tag mono">{{ asrTag }}</span>
      </div>
      <div class="row2">
        <div>
          <div class="row2-label">音频文件</div>
          <div class="row2-hint">WAV 会自动跳过 44 字节头。</div>
        </div>
        <div class="row2-ctl">
          <label class="btn sm" style="cursor: pointer">
            <Ico name="download" :size="13" />
            {{ busy === 'asr' ? '识别中…' : '选择音频文件' }}
            <input type="file" accept=".wav,.pcm,.raw,audio/*" style="display: none" @change="tryASR" />
          </label>
          <span v-if="asrFile" class="tiny mute mono truncate">{{ asrFile.name }} · {{ fmtBytes(asrFile.size) }}</span>
          <span v-else class="tiny mute">还没上传音频</span>
        </div>
      </div>
      <div v-if="asrResult" class="notice ok mt-10">
        <Ico name="check" />
        <div>
          <div class="strong">{{ asrResult }}</div>
          <div class="tiny mono mt-2">{{ asrInfo }}</div>
        </div>
      </div>
    </section>
  </div>
</template>
