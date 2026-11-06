<script setup>
// 助手 · 对话行为
// 全局策略：一轮对话什么时候结束、带多少记忆、允许几轮工具调用。
import { computed } from 'vue'
import Ico from '../components/Ico.vue'
import { state, go, revertSection } from '../store'

const d = computed(() => state.draft?.dialog || {})

const exitWordsText = computed({
  get: () => (d.value.exit_words || []).join('、'),
  set: (v) => {
    if (!state.draft?.dialog) return
    state.draft.dialog.exit_words = String(v)
      .split(/[、,，\s]+/)
      .map((x) => x.trim())
      .filter(Boolean)
  },
})
</script>

<template>
  <div class="sheet">
    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">退出词</div>
          <div class="sect-d">说出这些词就结束连续对话；档案里配了退出词则优先用档案的。</div>
        </div>
        <button class="btn sm ghost" @click="revertSection('dialog')">恢复上次保存</button>
      </div>
      <div class="rows">
        <div class="row2 stack">
          <div>
            <div class="row2-label">全局退出词</div>
            <div class="row2-hint">用顿号、逗号或空格分隔，例如：停止、退出、退下、再见。</div>
          </div>
          <div class="row2-ctl">
            <input v-model="exitWordsText" class="input" placeholder="停止、退出、退下、下去吧、别说了、再见、结束" />
          </div>
        </div>
      </div>
      <div class="chips mt-10">
        <span v-for="w in d.exit_words || []" :key="w" class="chip on">{{ w }}</span>
        <span v-if="!d.exit_words?.length" class="chip">还没有退出词</span>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">一轮对话的边界</div>
          <div class="sect-d">什么时候继续等、什么时候放手。单个助手可以在档案里覆盖这些值。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">连续对话超时</div>
            <div class="row2-hint">这么久没说话就退出，下次要重新唤醒。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="5" max="120" step="5" v-model.number="d.timeout_sec" />
              <span class="slider-value">{{ d.timeout_sec }}s</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">最多播几句</div>
            <div class="row2-hint">防止模型喋喋不休；超出直接截断。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="1" max="12" step="1" v-model.number="d.max_sentences" />
              <span class="slider-value">{{ d.max_sentences }} 句</span>
            </div>
          </div>
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">模型与工具轮次</div>
          <div class="sect-d">每多一轮工具调用，就是一次完整的模型往返。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">工具轮数上限</div>
            <div class="row2-hint">直接体现为变慢；3~4 轮足够家用场景。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="1" max="8" step="1" v-model.number="d.max_rounds" />
              <span class="slider-value">{{ d.max_rounds }} 轮</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">带入的历史消息</div>
            <div class="row2-hint">太多会拖慢模型并挤爆上下文，太少会失忆。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="0" max="60" step="2" v-model.number="d.max_history" />
              <span class="slider-value">{{ d.max_history }} 条</span>
            </div>
          </div>
        </div>
        <div class="row2">
          <div>
            <div class="row2-label">上下文保鲜期</div>
            <div class="row2-hint">隔这么久没说话，下次就当新对话：历史清空。0 = 永不过期。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="0" max="180" step="5" v-model.number="d.context_ttl_min" />
              <span class="slider-value">{{ d.context_ttl_min === 0 ? '不过期' : d.context_ttl_min + ' 分钟' }}</span>
            </div>
          </div>
        </div>
      </div>
    </section>

    <section class="sect">
      <div class="sect-h">
        <div class="grow">
          <div class="sect-t">音频下发</div>
          <div class="sect-d">网络抖动时攒一点再播，避免断续。</div>
        </div>
      </div>
      <div class="rows">
        <div class="row2">
          <div>
            <div class="row2-label">下发预缓冲</div>
            <div class="row2-hint">攒够这么多音频再推给设备；越大越稳、越小越跟手。</div>
          </div>
          <div class="row2-ctl">
            <div class="slider-row">
              <input type="range" min="40" max="400" step="20" v-model.number="d.prebuffer_ms" />
              <span class="slider-value">{{ d.prebuffer_ms }}ms</span>
            </div>
          </div>
        </div>
      </div>
      <div class="notice mt-10">
        <Ico name="clock" />
        <div class="grow">
          这一项和「语音 · 说与播报」里的「起播预缓冲」是两段：这里管设备侧播放下发，那里管服务端合成起播。
          不确定就都保持默认。
        </div>
        <button class="btn sm ghost" @click="go('voice', 'speak')">去看看</button>
      </div>
    </section>
  </div>
</template>
