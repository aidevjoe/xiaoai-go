<script setup>
import { computed, ref } from 'vue'
import Ico from '../components/Ico.vue'
import Field from '../components/Field.vue'
import { state, toast, go } from '../store'
import { api } from '../api'

const t = computed(() => state.draft?.tools || {})
const busy = ref('')
const testResult = ref(null)
const blockInput = ref('')

const toolNames = computed(() => state.meta.tool_names || [])
const toolLabels = {
  ha_search_entities: '搜索设备',
  ha_get_state: '查询状态',
  ha_call_service: '控制设备',
}
const toolDesc = {
  ha_search_entities: '按名字模糊找实体，不确定设备名时先查',
  ha_get_state: '看单个实体的详细状态（亮度、温度、当前模式）',
  ha_call_service: '真正下指令：开灯、调温度、开窗帘',
}

function enabled(name) {
  const list = t.value.enabled || []
  if (!list.length) return true // 空 = 全部启用
  return list.includes(name)
}

function toggle(name) {
  const all = toolNames.value
  let list = t.value.enabled || []
  if (!list.length) list = [...all] // 从"全部"开始改，先具体化
  list = list.includes(name) ? list.filter((n) => n !== name) : [...list, name]
  // 全选等价于"空"，语义更清楚
  t.value.enabled = list.length === all.length ? [] : list
}

function addBlock() {
  const v = blockInput.value.trim()
  if (!v) return
  if (!t.value.blocked) t.value.blocked = []
  if (!t.value.blocked.includes(v)) t.value.blocked.push(v)
  blockInput.value = ''
}
function rmBlock(d) {
  t.value.blocked = (t.value.blocked || []).filter((x) => x !== d)
}

async function test() {
  busy.value = 'test'
  testResult.value = null
  try {
    testResult.value = await api.toolsTest()
  } catch (e) {
    testResult.value = { ok: false, error: e.message }
  } finally {
    busy.value = ''
  }
}

const profileUsage = computed(() => state.draft?.profiles || [])

// 搜索触发词：逗号/顿号/空格分隔的字符串 ↔ 数组
const searchWords = computed({
  get: () => (t.value.search?.keywords || []).join('、'),
  set: (v) => {
    if (!t.value.search) return
    t.value.search.keywords = String(v)
      .split(/[、,，\s]+/)
      .map((x) => x.trim())
      .filter(Boolean)
  },
})

const searchBusy = ref(false)
const searchResult = ref(null)
async function testSearch() {
  searchBusy.value = true
  searchResult.value = null
  try {
    searchResult.value = await api.toolsTestSearch('今天的新闻')
  } catch (e) {
    searchResult.value = { ok: false, error: e.message }
  } finally {
    searchBusy.value = false
  }
}

const wxBusy = ref(false)
const wxResult = ref(null)
async function testWeather() {
  wxBusy.value = true
  wxResult.value = null
  try {
    wxResult.value = await api.toolsTestWeather()
  } catch (e) {
    wxResult.value = { ok: false, error: e.message }
  } finally {
    wxBusy.value = false
  }
}
</script>

<template>
  <div class="page">
    <!-- 表单类设置：一张面板 + 若干区块 + 行，不再每组各套一张卡片 -->
    <div class="sheet">
      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">Home Assistant 连接</div>
            <div class="sect-d">设备类工具的数据源；不填则工具自动降级为不可用。</div>
          </div>
          <span class="tag" :class="state.meta.tools_ready ? 'ok' : 'warn'">
            {{ state.meta.tools_ready ? '已连接' : '未配置' }}
          </span>
        </div>
        <div class="rows">
          <div class="row2">
            <div>
              <div class="row2-label">地址</div>
              <div class="row2-hint">带端口，如 http://homeassistant.local:8123。</div>
            </div>
            <div class="row2-ctl"><input v-model="t.ha_url" class="input mono" placeholder="http://homeassistant.local:8123" /></div>
          </div>
          <div class="row2">
            <div>
              <div class="row2-label">长期访问令牌</div>
              <div class="row2-hint">HA 个人资料页 → 安全 → 长期访问令牌。</div>
            </div>
            <div class="row2-ctl">
              <input v-model="t.ha_token" class="input mono" type="password" placeholder="eyJ0eXAiOiJKV1Qi…" autocomplete="off" />
            </div>
          </div>
        </div>
        <div class="flex items-center gap-10 mt-10">
          <button class="btn sm" :disabled="busy === 'test'" @click="test()">
            <Ico name="plug" :size="13" /> {{ busy === 'test' ? '测试中…' : '测试连接' }}
          </button>
          <span class="tiny mute">测试用的永远是已保存的配置</span>
        </div>
        <div v-if="testResult" class="notice mt-10" :class="testResult.ok ? 'ok' : 'bad'">
          <Ico :name="testResult.ok ? 'check' : 'close'" />
          <div v-if="testResult.ok">
            <div class="strong">连接正常 · {{ testResult.entities }} 个实体 · {{ testResult.ms }}ms</div>
            <div class="flex gap-6 wrap mt-6">
              <span v-for="d in testResult.domains.slice(0, 12)" :key="d.domain" class="tag">
                {{ d.domain }} <span class="mono mute">{{ d.count }}</span>
              </span>
            </div>
          </div>
          <div v-else>
            <div class="strong">连不上</div>
            <div class="mono small mt-2">{{ testResult.error }}</div>
            <div class="tiny mt-6">常见原因：地址少了 http://、端口不对、令牌过期、服务端与 HA 不在同一网段。</div>
          </div>
        </div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">联网搜索</div>
            <div class="sect-d">命中关键词时先搜一次，把结果拼进提示词——语音场景没耐心等两轮往返。</div>
          </div>
          <label class="switch"><input type="checkbox" v-model="t.search.enabled" /><span class="track"></span></label>
        </div>
        <template v-if="t.search?.enabled">
          <div class="rows">
            <div class="row2">
              <div>
                <div class="row2-label">服务商</div>
                <div class="row2-hint">博查在国内可直连；Tavily 需要能出网。</div>
              </div>
              <div class="row2-ctl">
                <select v-model="t.search.provider" class="input">
                  <option value="bocha">博查 Bocha（推荐）</option>
                  <option value="tavily">Tavily</option>
                  <option value="serper">Serper（Google）</option>
                </select>
              </div>
            </div>
            <div class="row2">
              <div>
                <div class="row2-label">API Key</div>
                <div class="row2-hint">只保存在本地配置文件里。</div>
              </div>
              <div class="row2-ctl"><input v-model="t.search.api_key" class="input mono" type="password" placeholder="sk-…" autocomplete="off" /></div>
            </div>
            <div class="row2">
              <div>
                <div class="row2-label">条数</div>
                <div class="row2-hint">越多越慢，3 条通常够。</div>
              </div>
              <div class="row2-ctl"><input type="number" min="1" max="8" step="1" v-model.number="t.search.max_results" class="input" style="max-width: 140px" /></div>
            </div>
            <div class="row2">
              <div>
                <div class="row2-label">超时</div>
                <div class="row2-hint">超时就当没搜到，不拖住回答。</div>
              </div>
              <div class="row2-ctl"><input type="number" min="300" max="5000" step="100" v-model.number="t.search.timeout_ms" class="input" style="max-width: 140px" /></div>
            </div>
            <div class="row2">
              <div>
                <div class="row2-label">触发词</div>
                <div class="row2-hint">说这些话才会搜；留空用内置默认。</div>
              </div>
              <div class="row2-ctl"><input v-model="searchWords" class="input" placeholder="最新、新闻、多少钱、价格" /></div>
            </div>
            <div class="row2">
              <div>
                <div class="row2-label">每轮都搜</div>
                <div class="row2-hint">会明显变慢，只在调试时开。</div>
              </div>
              <div class="row2-ctl"><label class="switch"><input type="checkbox" v-model="t.search.always" /><span class="track"></span></label></div>
            </div>
          </div>
          <div class="flex items-center gap-10 mt-10">
            <button class="btn sm" :disabled="searchBusy" @click="testSearch()">
              <Ico name="search" :size="13" /> {{ searchBusy ? '搜索中…' : '试搜一句' }}
            </button>
            <span class="tiny mute">测试用的永远是已保存的配置</span>
          </div>
          <div v-if="searchResult" class="notice mt-10" :class="searchResult.ok ? 'ok' : 'bad'">
            <Ico :name="searchResult.ok ? 'check' : 'close'" />
            <div class="grow">
              <div class="strong">{{ searchResult.ok ? '搜索正常 · ' + searchResult.ms + 'ms' : '搜索失败' }}</div>
              <div class="small mt-2" style="white-space: pre-wrap">{{ searchResult.ok ? searchResult.result || '(没有结果)' : searchResult.error }}</div>
            </div>
          </div>
        </template>
        <div v-else class="notice"><Ico name="info" /><div>未开启：模型只能凭训练数据回答。</div></div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">天气来源</div>
            <div class="sect-d">问到天气时提前取好塞进提示词；HA 的 weather 实体常常只有"晴/多云"。</div>
          </div>
        </div>
        <div class="rows">
          <div class="row2">
            <div>
              <div class="row2-label">来源</div>
              <div class="row2-hint">联网搜索用上面那组搜索配置。</div>
            </div>
            <div class="row2-ctl">
              <select v-model="t.weather.provider" class="input">
                <option value="search">联网搜索（覆盖面最广）</option>
                <option value="qweather">和风天气（最准，需要 key）</option>
                <option value="ha">Home Assistant（常常只有晴/多云）</option>
              </select>
            </div>
          </div>
          <div class="row2">
            <div>
              <div class="row2-label">预报天数</div>
              <div class="row2-hint">0 = 只说实况；要回答「明天要不要带伞」就填 3。</div>
            </div>
            <div class="row2-ctl"><input type="number" min="0" max="7" step="1" v-model.number="t.weather.days" class="input" style="max-width: 140px" /></div>
          </div>
          <template v-if="t.weather?.provider === 'qweather'">
            <div class="row2">
              <div><div class="row2-label">和风 API Key</div><div class="row2-hint">dev.qweather.com 免费注册。</div></div>
              <div class="row2-ctl"><input v-model="t.weather.api_key" class="input mono" type="password" placeholder="32 位 key" autocomplete="off" /></div>
            </div>
            <div class="row2">
              <div><div class="row2-label">LocationID</div><div class="row2-hint">城市 ID（北京 101010100）或 "经度,纬度"。</div></div>
              <div class="row2-ctl"><input v-model="t.weather.location" class="input mono" placeholder="101010100" /></div>
            </div>
          </template>
          <div v-else-if="t.weather?.provider === 'search'" class="row2">
            <div><div class="row2-label">城市</div><div class="row2-hint">搜索词会拼成「杭州 今天天气」。</div></div>
            <div class="row2-ctl"><input v-model="t.weather.location" class="input" placeholder="杭州" /></div>
          </div>
        </div>
        <div class="flex items-center gap-10 mt-10">
          <button class="btn sm" :disabled="wxBusy" @click="testWeather()">
            <Ico name="sun" :size="13" /> {{ wxBusy ? '读取中…' : '试读天气' }}
          </button>
          <span class="tiny mute">测试用的永远是已保存的配置</span>
        </div>
        <div v-if="wxResult" class="notice mt-10" :class="wxResult.ok ? 'ok' : 'bad'">
          <Ico :name="wxResult.ok ? 'check' : 'close'" />
          <div class="grow">
            <div class="strong">{{ wxResult.ok ? '天气正常 · ' + wxResult.ms + 'ms · ' + wxResult.provider : '读取失败' }}</div>
            <div class="small mt-2">{{ wxResult.ok ? wxResult.result : wxResult.error }}</div>
          </div>
        </div>
      </section>

      <section class="sect">
        <div class="sect-h">
          <div class="grow">
            <div class="sect-t">安全护栏</div>
            <div class="sect-d">语音识别会听错字，这些域一旦误触发后果不可逆，服务端直接拒绝。</div>
          </div>
        </div>
        <div class="row2 stack">
          <div>
            <div class="row2-label">禁止调用的域</div>
            <div class="row2-hint">建议保留 lock、alarm_control_panel、valve、shell_command。</div>
          </div>
          <div class="row2-ctl" style="flex-direction: column; align-items: stretch">
            <div class="chips">
              <span v-for="d in t.blocked || []" :key="d" class="chip on">
                {{ d }} <span style="cursor: pointer; opacity: 0.75" @click="rmBlock(d)">×</span>
              </span>
              <span v-if="!t.blocked?.length" class="chip">没有限制</span>
            </div>
            <div class="input-group mt-6">
              <input v-model="blockInput" class="input mono" placeholder="域名，如 lock / alarm_control_panel" @keyup.enter="addBlock()" />
              <button class="btn sm" @click="addBlock()">添加</button>
            </div>
          </div>
        </div>
      </section>
    </div>

    <!-- 列表类内容仍然是卡片：一行就是一个独立条目 -->
    <div class="card">
      <div class="card-head">
        <Ico name="sliders" />
        <div class="grow">
          <div class="card-title">工具开关</div>
          <div class="card-desc">全局开关；每个档案还能再单独收紧。关掉的工具模型连定义都看不到。</div>
        </div>
      </div>
      <div class="list" style="margin: -1px 0">
        <div v-for="n in toolNames" :key="n" class="row">
          <div class="row-main">
            <div class="flex items-center gap-6">
              <span class="strong small">{{ toolLabels[n] || n }}</span>
              <span class="tag mono tiny">{{ n }}</span>
            </div>
            <div class="tiny mute mt-2">{{ toolDesc[n] }}</div>
          </div>
          <label class="switch"><input type="checkbox" :checked="enabled(n)" @change="toggle(n)" /><span class="track"></span></label>
        </div>
      </div>
      <div class="card-foot">
        <span class="tiny mute">工具数量对首字延迟是数量级影响：实测 5 个工具首字 7.8 秒，2 个工具 1.0 秒。</span>
      </div>
    </div>

    <div class="card">
      <div class="card-head">
        <Ico name="users" />
        <div class="grow">
          <div class="card-title">各档案的工具范围</div>
          <div class="card-desc">档案里的设置还会再过滤一次，这里只是总览。</div>
        </div>
        <button class="btn sm ghost" @click="go('assistant', 'profiles')">去档案里改</button>
      </div>
      <div class="list">
        <div v-for="p in profileUsage" :key="p.id" class="row">
          <div class="pcard-avatar" style="width: 24px; height: 24px; font-size: 11px">{{ (p.name || 'A').slice(0, 1) }}</div>
          <div class="row-main truncate">{{ p.name || p.id }}</div>
          <span class="tag" :class="p.tools?.includes('none') ? 'warn' : 'info'">
            {{ !p.tools?.length ? '全部工具' : p.tools.includes('none') ? '不用工具' : p.tools.map((x) => toolLabels[x] || x).join('、') }}
          </span>
        </div>
      </div>
    </div>
  </div>
</template>
