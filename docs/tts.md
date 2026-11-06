# TTS 接入层设计

> 目标：**接一家新的语音平台 = 新增一个文件**，不改核心逻辑、不改配置结构、不改前端。
> 同时支持"运行时切换平台与音色"，并且全局配置与单个助手配置能叠加。

## 1. 为什么要重做

改造前，TTS 是**引擎的构造参数**：

```
loadEngine(settings) → NewBundle(TTS: newSherpaTTS(...))
```

于是：

- 换平台 = 重新装配整个引擎（ASR/VAD/KWS 一起重载，几秒 + 几百 MB 内存抖动）；
- 音色是**实例状态**，多个助手想要不同声音只能反复 `SetSpeed/SetSpeaker` 改同一份对象，
  并发两路对话会互相覆盖（原代码里就写着"这个近似够用"）；
- 接新平台要动三处：配置结构、装配代码、后台页面。

现在的分层：

```
                    ┌──────────────────────────────────────┐
   对话层           │ dialog.Manager                       │
   （不认识平台）    │   SpeakOptions.Voice ──┐             │
                    └────────────────────────┼─────────────┘
                                             │ audio.Speaker
                    ┌────────────────────────▼─────────────┐
   路由层           │ audio.Service                        │
   （只认注册表）    │   Resolve → Plan {provider,voice,…}  │
                    │   实例池（LRU） + 复用引擎里的那一份   │
                    └────────────────────────┬─────────────┘
                                             │ audio.Synthesizer
                    ┌────────────────────────▼─────────────┐
   供应商层         │ doubao │ edge │ http │ sherpa │ …    │
   （各自一个文件）  │ Fields │ Voices │ Clone │ New       │
                    └──────────────────────────────────────┘
```

## 2. 三层职责

### 2.1 供应商层：`TTSProvider`

一个供应商 = 一个工厂 + 三份**声明式元数据**：

```go
type TTSProvider struct {
    Name, Desc string
    Local      bool
    New        func(TTSOptions) (Synthesizer, error)

    Fields   []Field                              // 需要哪些配置项
    Voices   []Voice                              // 内置音色清单
    Discover func(ctx, cfg) ([]Voice, error)      // 可选：在线拉全量
    Clone    *CloneSpec                           // 可选：声音复刻
}
```

关键点：**元数据是给后台用的，不是给核心用的**。核心只调 `New`。

`Field.Kind` 只有六种（text/password/number/select/bool/textarea），
足够覆盖目前见过的所有平台参数；后台据此渲染控件，因此"加一个配置项"
不需要动前端。

### 2.2 路由层：`audio.Service`

按 `VoiceChoice{Provider, Voice, Params, Speed}` 决定这一句用谁合成。四件事：

**① 合并配置（`Resolve`）**

```
provider  ← 档案 > 全局
params    ← 该平台的全局配置 > 档案覆盖
voice     ← 档案 > 全局（同平台时）> params["voice"] > 平台默认
speed     ← 档案 > 全局 > 1.0
```

"该平台的全局配置"是重点：用户在一个地方配一次豆包 key，所有助手都能用；
档案切到 Edge 时自动取 Edge 自己那份配置，**不会**把豆包的 key 发给微软。

**② 实例池**

- 键 = `provider + hash(params)`，与音色无关（音色是请求参数）；
- LRU，上限 8 个，淘汰时 `Close`；
- 云端 provider 的 `New` 只是建一个 `http.Client`，代价可以忽略；
  本地模型才有"重载几百毫秒"的问题，所以下面这条更重要：

**③ 复用引擎里的实例**

引擎仍然按全局配置装一份 TTS。如果某次请求的 `provider+params` 与引擎一致，
直接调引擎（`Bundle.SynthesizeWith`），**不重复加载模型**。
判断依据是引擎实际装上的 provider（`loadEngine` 的返回值），不是配置里的值——
配置写 `doubao` 但装配失败回退成 `sherpa` 时，两者不一样。

**④ 音色作为请求参数**

```go
type SynthOptions struct{ Text, Voice string; Speed float32 }
type VoiceSynthesizer interface {
    SynthesizeWith(ctx context.Context, o SynthOptions) ([]byte, error)
}
```

实现了它就永远只保留一份实例（抖音/Edge 都是 HTTP 语义，天然无状态）；
没实现（本地 VITS，音色由模型文件决定）就退化成"按音色分实例"，仍然能工作。
这是**可选接口**而不是必选：老的 provider 一行都不用改。

### 2.3 对话层：`audio.Speaker`

播放器只需要"把一句话变成 PCM + 采样率"：

```go
type Speaker interface {
    Synthesize(ctx, text string, v VoiceChoice) (pcm []byte, rate int, err error)
    Speed() float32
    SetSpeed(float32)
    Available() bool
    Status() string
}
```

`Service` 和 `EngineSpeaker(eng)` 都实现它，所以"只有本地引擎"和"多供应商路由"
可以互换，`speaker.go` 一行都不用改。`Manager.SetSpeech` 注入后者时会把
`speechExplicit` 置位，`SetEngine` 就不会再把路由层顶掉。

采样率**跟着每一段音频返回**，而不是建播放器时取一次：同一次回答里换音色/换平台
会让采样率不同（本地 8k、豆包 24k、Edge 24k），用固定值会播成怪声。

## 3. 新增一家平台：完整清单

1. 新建 `internal/server/audio/tts_xxx.go`：
   - 实现 `Synthesizer`（+ 可选 `VoiceSynthesizer`）；
   - `init()` 里 `RegisterTTS(...)`，填 `Fields` / `Voices` / `Discover` / `Clone`。
2. 需要复刻的话，实现 `CloneSpec` 三个函数（见下）。
3. 什么都不用改：下拉框、表单、音色选择、试听、复刻面板全部自动出现。

对照实现：

| 文件 | 平台 | 特点 |
| --- | --- | --- |
| `tts_doubao.go` | 豆包 v3 + v1 | 双版本、流式响应解析（chunked/SSE 都吃）、base64 音频 |
| `tts_doubao_clone.go` | 豆包复刻 | v3 `voice_clone` / `get_voice` |
| `tts_edge.go` | Edge | WebSocket + Sec-MS-GEC 时间令牌 + SSML + MP3 解码 |
| `tts_http.go` | 通用 HTTP | 纯配置，字段名可改以适配任意壳子 |

## 4. 声音复刻的集成方案

复刻的本质是"**先有音色，再有合成**"。各家流程高度一致，所以抽成三段：

```go
type CloneSpec struct {
    Fields []Field                                    // 语种、降噪、训练模型…
    Upload func(ctx, cfg, CloneRequest) (CloneState, error)  // 提交样本 → speaker_id
    Query  func(ctx, cfg, speakerID) (CloneState, error)     // 轮询训练状态
    List   func(ctx, cfg) ([]Voice, error)                   // 列出现有复刻音色
}
```

为什么是三个函数而不是一个"克隆并等待"：

- 训练是异步的，可能几分钟；HTTP 请求扛不住那么久，必须让前端自己轮询；
- 轮询要能在页面刷新后继续（状态存在平台侧，服务端不持有任务）。

`CloneState.Status` 统一成 5 个值（notfound/training/ready/failed/active），
所以后台的进度提示与豆包的数字码解耦。

### 豆包的具体接口

| 用途 | 接口 |
| --- | --- |
| 提交训练 | `POST https://openspeech.bytedance.com/api/v3/tts/voice_clone` |
| 查询状态 | `POST https://openspeech.bytedance.com/api/v3/tts/get_voice` |
| 用复刻音色合成 | `POST …/api/v3/tts/unidirectional`，`speaker=S_xxx`，`X-Api-Resource-Id: seed-icl-2.0` |

鉴权两种都发（服务端认哪个都行）：
新版控制台 `X-Api-Key`；旧版 `X-Api-App-Key` + `X-Api-Access-Key`。

训练状态码：`0 NotFound / 1 Training / 2 Success / 3 Failed / 4 Active`，
2 和 4 都能直接拿去合成。

### 在通用架构里，复刻只产出一个"音色 ID"

这是设计上最关键的一点：**复刻不引入新的代码路径**。

```
复刻前：Voices = [官方音色…]
复刻后：Voices = [官方音色…, {ID:"S_xxx", Name:"我的声音", Cloned:true}]
```

合成时它就是一个普通的 `speaker` 参数。由此带来三个好处：

- 档案里选复刻音色、全局默认用复刻音色、试听、固定应答缓存，全都自动支持；
- 平台不提供"列出音色"接口时（豆包就没有公开的列表接口），
  仍可用通用的 `clone_voices` 配置手工登记：`"S_xxx:我的声音,S_yyy:老婆的声音"`；
- 未来接别家复刻（Azure Custom Neural Voice、MiniMax 等），
  只要它的产出也是一个音色 ID，就同样不需要改核心。

### 唯一需要人工记住的坑

复刻音色必须配 `resource_id=seed-icl-2.0`（声音复刻大模型）。
用 `seed-tts-2.0` 会报"音色不存在"。后台点「用这个音色」时会自动切换这一项，
并在提示里说明原因——这是踩过之后加的护栏。

## 5. 试听与排错

`POST /api/tts/preview` 用"已保存的配置 + 请求里的覆盖项"合成一句话并返回 WAV，
所以能在保存前先听。两个细节：

- **静音检测**：部分平台在音色 ID 写错时返回 200 + 一段静音，光看状态码发现不了。
  `audio.HasAudio` 判一次峰值，静音就明确提示"检查音色 ID 与模型版本"。
- **凭据不出现在 URL 上**：音色清单接口从服务端配置读凭据，前端只传 provider 名。

音色清单的取法（三家平台差异最大的一环）：

| 平台 | 内置 | 在线拉取 | 兜底 |
| --- | --- | --- | --- |
| Edge | 12 个常用中文 | ✅ 322 个（老域名 `voices/list`，新域名已 403） | — |
| 豆包 | 6 个确认存在的 | ❌ 无公开接口 | 后台「自定义音色清单」粘贴 ID |
| http | 无 | 无 | 同上 |

拉到的全量清单缓存在 `data/tts-voices/<provider>.json`（7 天），不进 `settings.json`——
否则配置文件会被 300 多行音色 ID 淹掉，而且每次打开后台都要整份传一遍。

Edge 的两个常见错误也翻译成人话：

| 现象 | 真实原因 |
| --- | --- |
| 403 拒绝握手 | 本机时钟不准（`Sec-MS-GEC` 是按 5 分钟窗口对时间戳做 SHA-256） |
| 429 限流 | 免费服务对出口 IP 的限制，等几秒或换平台 |

## 6. 取舍

| 决定 | 理由 |
| --- | --- |
| 音色走请求参数，不改实例状态 | 多助手并发不串音；换音色零成本 |
| 实例池上限 8 且 LRU | 家庭场景最多 3~4 个组合；上限是防御性的 |
| 引擎里的那份实例优先复用 | 本地模型重复加载会翻倍占内存 |
| 平台级参数单独存一份 | 密钥是账号级的，逼用户每个助手抄一遍是设计错误 |
| 复刻只产出音色 ID | 不为特殊能力开特殊路径，否则每接一家都要改核心 |
| 不自动重试 429/超时 | 语音是实时交互，重试只会让用户等更久；失败就走提示音 + 错误应答 |
