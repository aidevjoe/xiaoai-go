<script setup>
import { computed, ref } from 'vue'
import Ico from '../components/Ico.vue'
import Field from '../components/Field.vue'
import { state, toast, loadAll, refreshLive, replaceDraft } from '../store'
import { api } from '../api'
import { fmtBytes, fmtDur } from '../utils'

const s = computed(() => state.draft?.server || {})
const stats = computed(() => state.stats)
const busy = ref('')
const recs = ref([])
const recDir = ref('')

const shellScript = ref('cat /proc/loadavg')
const shellOut = ref('')

async function loadRecs() {
  busy.value = 'recs'
  try {
    const r = await api.recordings()
    recs.value = r.items || []
    recDir.value = r.dir || ''
  } catch (e) {
    toast(e.message, 'bad')
  } finally {
    busy.value = ''
  }
}
loadRecs()

async function clearTurns() {
  if (!confirm('清空内存里的对话记录？此操作不影响已保存的配置文件。')) return
  try {
    await api.clearTurns()
    state.turns = []
    toast('对话记录已清空', 'ok')
  } catch (e) {
    toast(e.message, 'bad')
  }
}

async function reloadEngine() {
  busy.value = 'engine'
  try {
    const r = await api.engineReload()
    toast(`引擎已重载（${r.ms}ms）`, 'ok')
    await refreshLive()
  } catch (e) {
    toast(e.message, 'bad', 6000)
  } finally {
    busy.value = ''
  }
}

function exportConfig() {
  const blob = new Blob([JSON.stringify(state.saved, null, 2)], { type: 'application/json' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = 'xiaoai-go-settings.json'
  a.click()
  URL.revokeObjectURL(a.href)
  toast('已导出当前配置', 'ok')
}

async function importConfig(ev) {
  const f = ev.target.files?.[0]
  if (!f) return
  try {
    const text = await f.text()
    const obj = JSON.parse(text)
    replaceDraft(obj)
    toast('已导入并保存', 'ok', 5000)
  } catch (e) {
    toast('不是合法 JSON：' + e.message, 'bad', 5000)
  } finally {
    ev.target.value = ''
  }
}

async function runShell() {
  const dev = state.devices[0]
  if (!dev) {
    toast('没有设备在线', 'warn')
    return
  }
  if (!confirm(`在设备 ${dev.id} 上执行：\n\n${shellScript.value}`)) return
  busy.value = 'shell'
  shellOut.value = ''
  try {
    const r = await api.shell(dev.id, shellScript.value)
    const res = r.result || {}
    shellOut.value = `exit=${res.exit_code}\n${res.stdout || ''}${res.stderr ? '\n[stderr] ' + res.stderr : ''}`
  } catch (e) {
    shellOut.value = '执行失败：' + e.message
  } finally {
    busy.value = ''
  }
}

async function factoryReset() {
  if (!confirm('丢弃当前未保存的改动，重新从服务器载入配置？')) return
  await loadAll()
  toast('已重新从服务器加载', 'ok')
}
</script>

<template>
  <div class="page">
    <!-- 运行指标是 KPI 卡片，和下面的设置面板刻意区分开 -->
    <div class="grid-4">
      <div class="metric">
        <div class="metric-label">版本</div>
        <div class="metric-value" style="font-size: 15px">{{ state.meta.version || '—' }}</div>
        <div class="tiny mute mono">{{ state.meta.go }}</div>
      </div>
      <div class="metric">
        <div class="metric-label">运行时长</div>
        <div class="metric-value" style="font-size: 15px">{{ fmtDur(stats.uptime_sec) }}</div>
        <div class="tiny mute">{{ stats.goroutines }} 协程</div>
      </div>
      <div class="metric">
        <div class="metric-label">内存</div>
        <div class="metric-value" style="font-size: 15px">{{ stats.heap_mb }}<small>MB</small></div>
        <div class="tiny mute">向系统申请 {{ stats.sys_mb }} MB</div>
      </div>
      <div class="metric">
        <div class="metric-label">上行音频</div>
        <div class="metric-value" style="font-size: 15px">{{ (stats.audio_kbps || 0).toFixed(1) }}<small>kbps</small></div>
        <div class="tiny mute">{{ fmtBytes(stats.audio_bytes) }}</div>
      </div>
    </div>

    <div class="sheet">
      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">身份与接入</div>
            <div class="sect-d">后台标题与设备侧鉴权。监听地址来自启动参数，界面上不能改。</div>
          </div>
        </div>
        <div class="rows">
          <div class="row2">
            <div><div class="row2-label">后台名称</div><div class="row2-hint">侧栏顶部显示的名字。</div></div>
            <div class="row2-ctl"><input v-model="s.name" class="input" placeholder="小爱家庭助手" /></div>
          </div>
          <div class="row2">
            <div>
              <div class="row2-label">设备接入令牌</div>
              <div class="row2-hint">留空 = 不校验；改动后已连接的设备不受影响，新连接开始校验。</div>
            </div>
            <div class="row2-ctl"><input v-model="s.device_token" class="input mono" type="password" placeholder="留空则不校验" autocomplete="off" /></div>
          </div>
          <div class="row2">
            <div><div class="row2-label">监听地址</div><div class="row2-hint">改这两个值会把自己断掉，所以只读。</div></div>
            <div class="row2-ctl">
              <input :value="state.meta.device_addr" class="input mono" readonly style="max-width: 160px" />
              <input :value="state.meta.admin_addr" class="input mono" readonly style="max-width: 160px" />
            </div>
          </div>
        </div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">记录与录音</div>
            <div class="sect-d">对话轮数只在内存里，重启即清空；原始录音可以落盘用于排查识别错误。</div>
          </div>
        </div>
        <div class="rows">
          <div class="row2">
            <div><div class="row2-label">保留对话轮数</div><div class="row2-hint">配置文件不含对话内容。</div></div>
            <div class="row2-ctl">
              <div class="slider-row">
                <input type="range" min="20" max="2000" step="20" v-model.number="s.keep_turns" />
                <span class="slider-value">{{ s.keep_turns }} 轮</span>
              </div>
            </div>
          </div>
          <div class="row2">
            <div><div class="row2-label">保存原始录音</div><div class="row2-hint">识别错的时候，回放听一遍比看日志有用得多。注意占空间。</div></div>
            <div class="row2-ctl"><label class="switch"><input type="checkbox" v-model="s.save_audio" /><span class="track"></span></label></div>
          </div>
          <div v-if="s.save_audio" class="row2">
            <div><div class="row2-label">录音目录</div><div class="row2-hint">留空默认放在配置文件同级的 audio/。</div></div>
            <div class="row2-ctl"><input v-model="s.audio_dir" class="input mono" placeholder="(默认) audio/" /></div>
          </div>
        </div>
        <div class="subcard mt-10">
          <div class="subcard-head">
            <span class="tiny mute mono grow truncate">{{ recDir || '尚未设置目录' }}</span>
            <button class="btn sm" :disabled="busy === 'recs'" @click="loadRecs()"><Ico name="refresh" :size="13" /> 刷新录音</button>
          </div>
          <div v-if="recs.length" class="list" style="max-height: 220px; overflow: auto">
            <div v-for="r in recs" :key="r.name" class="row">
              <Ico name="wave" :size="13" />
              <div class="row-main truncate tiny mono">{{ r.name }}</div>
              <span class="tag mono">{{ fmtBytes(r.size) }}</span>
              <audio :src="`/api/recordings/${encodeURIComponent(r.name)}`" controls style="height: 26px; width: 150px"></audio>
            </div>
          </div>
          <div v-else class="tiny mute">{{ s.save_audio ? '还没有录音' : '打开「保存原始录音」后才会记录' }}</div>
        </div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">配置文件</div>
            <div class="sect-d">全部配置都在一个 JSON 里，方便备份和版本管理。</div>
          </div>
          <span class="tag mono">{{ state.meta.config_path }}</span>
        </div>
        <div class="flex items-center gap-8 wrap">
          <button class="btn sm" @click="exportConfig()"><Ico name="download" :size="13" /> 导出</button>
          <label class="btn sm" style="cursor: pointer">
            <Ico name="download" :size="13" /> 导入并保存
            <input type="file" accept=".json,application/json" style="display: none" @change="importConfig" />
          </label>
          <button class="btn sm ghost" @click="factoryReset()"><Ico name="refresh" :size="13" /> 从服务器重载</button>
        </div>
        <div class="notice info mt-10">
          <Ico name="check" />
          <div>导入会直接保存并生效（校验不通过会退回来，不会写坏配置）。改错了可以点「从服务器重载」拿回服务器上的版本。</div>
        </div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">维护操作</div>
            <div class="sect-d">不会重启进程，都是热操作。</div>
          </div>
        </div>
        <div class="flex items-center gap-8 wrap">
          <button class="btn sm" :disabled="busy === 'engine'" @click="reloadEngine()">
            <Ico name="refresh" :size="13" /> {{ busy === 'engine' ? '重载中…' : '重载语音引擎' }}
          </button>
          <button class="btn sm danger" :disabled="!state.turns.length" @click="clearTurns()">
            <Ico name="trash" :size="13" /> 清空对话记录
          </button>
        </div>
        <div class="notice mt-10">
          <Ico name="clock" />
          <div>重载引擎会释放旧模型再加载新的：期间几百毫秒不可用，内存峰值约翻一倍。改完模型路径或线程数后做一次即可。</div>
        </div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">设备命令行</div>
            <div class="sect-d">高级：直接在音箱上跑 shell，排查问题用。</div>
          </div>
          <span class="tiny mute">目标：{{ state.devices[0]?.id || '无在线设备' }}</span>
        </div>
        <div class="row2 stack">
          <div><div class="row2-label">命令</div><div class="row2-hint">执行前会二次确认。</div></div>
          <div class="row2-ctl"><textarea v-model="shellScript" class="textarea mono" rows="3"></textarea></div>
        </div>
        <div class="flex items-center gap-8 mt-10">
          <button class="btn sm danger" :disabled="!state.devices.length || busy === 'shell'" @click="runShell()">
            {{ busy === 'shell' ? '执行中…' : '执行' }}
          </button>
        </div>
        <pre v-if="shellOut" class="mono tiny" style="margin: 10px 0 0; padding: 10px; background: var(--surface-2); border: 1px solid var(--line); border-radius: 9px; max-height: 200px; overflow: auto; white-space: pre-wrap">{{ shellOut }}</pre>
      </section>
    </div>
  </div>
</template>
