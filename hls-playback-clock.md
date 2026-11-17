# HLS 客户端播放时基、音画同步与时间戳回绕的静态分析

分析对象：本仓库 `github.com/bluenviron/gohlslib/v2`（分支 `task/53`，HEAD `b771acf`）的
**client** 实现与测试，以及其时间戳解码所依赖的 `github.com/bluenviron/mediacommon/v2 v2.10.0`
（`go.mod:8`、`go.sum`）。mediacommon 的源码取自本地 v2.10.0 工作树
（`pkg/formats/mpegts/time_decoder.go`、`pkg/formats/fmp4/*`），与 `go.sum` 记录的版本一致。

本文只做**静态分析**：阅读源码、测试与常量，不运行播放、不下载媒体。凡标注"待补证据"之处，
表示该结论依赖仓库外的实现或缺少测试覆盖。

---

## 0. 结论摘要

1. **共同播放基准**由 `clientTimeConv` 一族在**引导轨（leading track）的第一条样本**处建立：
   MPEG-TS 取该样本的原始 DTS 作为 `startDTS`（`client_stream_processor_mpegts.go:236-239`、
   `client_time_conv_mpegts.go:23-28`），fMP4 取首个分段首个 part 的引导轨 `tfdt BaseTime`
   与该轨 `mdhd timescale` 作为原点（`client_stream_processor_fmp4.go:229-242`）。
   伴随音轨不是各自建立基准，而是**复用引导流的同一个 `timeConv` 对象**：视频流在
   `setTimeConv` 时写入 `clientPrimaryDownloader`（`client_stream_processor_mpegts.go:328`、
   `client_primary_downloader.go:151-161`），音频 rendition 通过 `waitTimeConv` 取回
   （`client_stream_processor_mpegts.go:330-338`、`client_stream_processor_fmp4.go:243-253`）。
   fMP4 下不同时钟频率通过 `convert(v, clockRate) = v − leadingBaseTime·clockRate/leadingTimeScale`
   把引导轨原点换算到本轨单位（`client_time_conv_fmp4.go:26-28`），因此**各轨保留其相对引导轨
   的真实偏移**，而不是各自归零。
2. **分段连续切换后的音画偏移**不靠播放列表维持，而由三层状态共同保持：连续的 33 位展开器
   `mpegts.TimeDecoder`（跨分段累积，`client_time_conv_mpegts.go:15,34`）、固定的原点
   `startDTS`/`leadingBaseTime`（只在首样本设置一次）、以及所有轨道共享的
   `clientTrack.startSystem`（`client_primary_downloader.go:154-158`、`client_track.go:36-49`）。
   分段边界不重置任何一项；播放列表只提供可选的绝对时间锚点（PDT）。
3. **解码时间与呈现时间分工明确**：DTS（或 fMP4 的 `tfdt BaseTime` 递推值）用于时基换算、
   投递节拍与绝对时间外推；PTS 用于呈现，并且是 `pts < 0` 丢弃判定的唯一依据；fMP4 中
   `pts = dts + PTSOffset` 且 DTS 由 `BaseTime + ΣDuration` 递推
   （`client_track_processor_fmp4.go:114-135`），MPEG-TS 中 PTS/DTS 直接来自 PES
   （`mediacommon/pkg/formats/mpegts/reader.go:611-630`）。
4. **数值回绕与真实不连续是两件事**：MPEG-TS 的 33 位（90kHz≈26.5h）回绕由 `TimeDecoder` 按
   "半量程判正负"展开为 64 位（`time_decoder.go:36-46`）；fMP4 的 `BaseTime` 是 `uint64`
   （`parts.go:106`、`part_track.go:41-47`），不存在周期性回绕，跨轨差异只是**时间单位换算**。
   现有状态只对"单条连续时间轴 + 稳定原点"成立；原点归零或引导基准改变时，**没有任何重置逻辑**，
   `EXT-X-DISCONTINUITY` 虽被解析却从未被消费（`pkg/playlist/media.go:243-244`）。
5. **缺少播放日期信息只影响绝对时间，不影响媒体时间**：`setLeadingNTPReceived` 无条件关闭通道
   （`client_stream_processor_mpegts.go:288`），`getNTP` 在 `ntpAvailable=false` 时返回 `nil`，
   于是 `AbsoluteTime()` 返回 `(zero, false)`（`client_track.go:16-21`、`client.go:275-278`）；
   pts/dts 与节拍完全不依赖 PDT。

---

## 1. 播放时基如何形成

### 1.1 引导轨的选择

| 容器 | 选择规则 | 位置 |
|---|---|---|
| MPEG-TS | 优先首个 H264 轨；否则首个非 KLV 轨；否则第 0 轨 | `client_stream_processor_mpegts.go:17-34` |
| fMP4 | 优先首个 `IsVideo()` 轨；否则第 0 轨 | `client_stream_processor_fmp4.go:15-25` |

MPEG-TS 是在**同一个复用流**内选轨（所有轨同属一个 `clientStreamProcessorMPEGTS`）；
fMP4 则在 `init` 的 `Tracks` 内选，跨 rendition 时由视频流担任引导
（`client_primary_downloader.go:216-293`：leading 流 `isLeading=true`，音频 rendition
`isLeading=false`）。

### 1.2 MPEG-TS：首个引导样本的 DTS 即原点

处理路径（`client_stream_processor_mpegts.go:223-299`）：

1. 每条样本先经过 `processSample`。若 `p.timeConv == nil`：
   - 若该样本属于引导轨，调用 `initializeTimeConv(rawDTS)`；
   - 否则把样本闭包压入 `queuedSamples`（上限 `clientMaxMPEGTSQueuedSamples=1000`，
     `client.go:24`），等引导样本到达后再执行。
2. `initializeTimeConv`（`:321-341`）在 leading 流中新建 `clientTimeConvMPEGTS{startDTS: rawDTS}`；
   非 leading 流则 `waitTimeConv` 并**复用同一对象**，类型不符时报
   `"stream playlists are mixed MPEGTS/FMP4"`（`:335-338`）。
3. `clientTimeConvMPEGTS.initialize()`（`client_time_conv_mpegts.go:23-28`）：
   ```go
   ts.td = &mpegts.TimeDecoder{}; ts.td.Initialize()
   ts.startDTS = ts.td.Decode(ts.startDTS)   // 首次 Decode 返回原值
   ```
4. `convert(v)`（`:30-35`）：
   ```go
   return ts.td.Decode(v) - ts.startDTS
   ```
   即：**33 位展开后的值减去固定原点**，单位统一为 90kHz。MPEG-TS 的所有轨道 `ClockRate`
   都硬编码为 90000（`client_stream_processor_mpegts.go:175-180`）。

测试中的数值可直接验证这条链路（`client_test.go:203-246` 写入，`:1122-1171` 断言）：

| 样本 | 原始 PTS / DTS | 换算后 PTS / DTS | 说明 |
|---|---|---|---|
| 视频样本 0 | PTS=90000（+1s）, DTS=8589844592（=2³³−90000，即 −1s） | PTS=180000, DTS=0 | 原点=首条 DTS（首次 `Decode` 原值返回）；PTS 与 DTS 的模差 180000 |
| 视频样本 1 | PTS=93000, DTS=8589844592+3000 | PTS=183000, DTS=3000 | 模差累积 |
| 分段 2 视频 | DTS=8589844592+6000 | DTS=6000 | **原点未变、展开器跨分段继续累积** |
| 音频样本 0 | PTS=8589844592 | PTS=DTS=0 | AAC 无独立 DTS，`OnDataMPEG4Audio` 传入 `pts,pts`（`:210`） |
| 音频样本 1 | PTS=8589844592+3000 | PTS=3000 | |

（原始值见 `client_test.go:211-243`；`8589844592 = 2³³ − 90000 = 0x1FFFFFFFF − 90000 + 1`。）

`client_test.go:2298-2305`（ByteRange 用例）进一步证明：同一 `clientTimeConvMPEGTS` 在
"分段 1 起始 90000、分段 2 起始 180000"时输出 `dts=0` 与 `dts=90000`——原点固定，跨分段连续。

值得注意的是，上述"归一化后 dts=0 / pts=180000"的断言位于 `TestClient` 的公共回调中
（`client_test.go:1122-1171`），对 **mpegts、fmp4_singleplaylist、fmp4_multiplaylist、
lowlatency_singleplaylist** 四种变体**同时生效**。也就是说，测试把"MPEG-TS 用 2³³ 邻域的
原始时间戳、fMP4 用 `BaseTime=6s` + `PTSOffset=2s`"两条截然不同的输入，锁定到同一组输出。
这正是"容器时间表示不同、播放时基语义相同"的回归基准。

### 1.3 fMP4：引导轨 BaseTime 与 timescale 决定原点，跨轨按单位换算

fMP4 的轨道 `ClockRate` 直接取 init 中每轨的 `mdhd.TimeScale`
（`client_stream_processor_fmp4.go:81-104`；解析见 `mediacommon/pkg/formats/fmp4/init.go:150`），
因此同一流内可以同时存在 90000（视频）、44100（AAC）、48000（Opus）等不同单位。

`initializeTimeConv`（`client_stream_processor_fmp4.go:229-242`）：

```go
timeScale := findTimeScaleOfLeadingTrack(p.init.Tracks, p.leadingTrackID)
p.timeConv = &clientTimeConvFMP4{
    leadingTimeScale: int64(timeScale),
    leadingBaseTime:  int64(leadingPartTrack.BaseTime),
}
```

`convert`（`client_time_conv_fmp4.go:26-28`）：

```go
return v - multiplyAndDivide(ts.leadingBaseTime, int64(clockRate), ts.leadingTimeScale)
```

即：**把引导轨原点换算到本轨的时钟单位后，从本轨时间戳中减去**。
`multiplyAndDivide` 为整除（`muxer_segmenter.go:19-23`），因此换算是精确到 1 个本轨 tick 的
截断，而不是简单归零。

用测试数值验证（`client_test.go:337-410` 写入，`:1122-1171` 断言）：

| 轨 | init timescale | part BaseTime | 换算原点 | 样本 0 的 DTS/PTS |
|---|---|---|---|---|
| 视频（引导） | 90000 | 540000（6s） | 540000 | DTS=0, PTS=0+180000=180000（`PTSOffset=2s`） |
| 音频 | 44100 | 264600（6s） | 540000·44100/90000 = 264600 | DTS=PTS=0 |
| 音频样本 1 | 44100 | — | — | DTS=PTS=1470（=44100/30） |

可见两条时间轴在**同一绝对媒体时刻 6s** 上对齐，客户端只保留"相对引导轨原点"的偏移。
若音频 rendition 的首个 `BaseTime` 是 6.5s（286650），换算结果为 22050（0.5s），
客户端会保留这 0.5s 偏移；若音频更早（如 5.5s），换算结果为负，随后被丢弃（见 §1.5）。
**这意味着跨 rendition 对齐依赖生产者在 tfdt 中使用公共媒体时间轴**，客户端无法自行发现违约。

### 1.4 绝对时间锚点（PROGRAM-DATE-TIME）

MPEG-TS（`client_stream_processor_mpegts.go:282-291`）：

```go
if !p.dateTimeProcessed && p.isLeading && isLeadingTrack {
    p.dateTimeProcessed = true
    if p.curSegment.dateTime != nil {
        p.timeConv.setNTP(*p.curSegment.dateTime, dts)   // dts 已换算
    }
    p.timeConv.setLeadingNTPReceived()
}
ntp := p.timeConv.getNTP(errGroupCtx, dts)
```

fMP4（`client_stream_processor_fmp4.go:160-181`）：

```go
if p.isLeading {
    if seg.dateTime != nil {
        dts := p.timeConv.convert(int64(leadingPartTrack.BaseTime), leadClockRate)
        p.timeConv.setNTP(*seg.dateTime, dts, leadClockRate)
    }
    p.timeConv.setLeadingNTPReceived()
}
...
dts := p.timeConv.convert(int64(partTrack.BaseTime), trackClockRate)
ntp := p.timeConv.getNTP(errGroupCtx, dts, trackClockRate)
```

要点：

- **锚点用的是换算后的 DTS**，不是原始时间戳；`getNTP` 做同样的换算并线性外推
  （`client_time_conv_mpegts.go:69`、`client_time_conv_fmp4.go:63-66`）。
- fMP4 的 `getNTP` 会先把锚点时间戳按 `ntpClockRate` 换算到查询轨单位再求时长
  （`client_time_conv_fmp4.go:64-66`），因此跨时钟频率的绝对时间一致到 1 tick 以内：
  视频 3000/90000 与音频 1470/44100 都得到 33,333,333ns。
- **每个分段都重新锚定一次**：`dateTimeProcessed` 在 `processSegment` 开头重置
  （`client_stream_processor_mpegts.go:100`），fMP4 在每段开头按 `seg.dateTime` 重新 `setNTP`
  （`client_stream_processor_fmp4.go:160-168`）。
  因此 PDT 存在时，分段边界会纠正绝对时间漂移；PDT 缺失时保留上一次锚点继续外推。
- **只有 leading 流设置 PDT**（`p.isLeading`）。测试故意给音频 rendition 一个不同的 PDT
  （`2014-02-05`，`client_test.go:469`、`client_test.go:637`），断言音频绝对时间仍是视频的
  `2015-02-05T01:02:02Z`（`:1160-1163`、`:1182-1184`）——证明音频 rendition 的 PDT 被忽略，
  绝对时间完全跟随引导流。

### 1.5 解码时间与呈现时间的分工

| 用途 | 使用的时间 | 位置 |
|---|---|---|
| 时基换算 | DTS（MPEG-TS `rawDTS`；fMP4 `BaseTime` 与 `ΣDuration`） | `client_stream_processor_mpegts.go:280`、`client_stream_processor_fmp4.go:180`、`client_track_processor_fmp4.go:114-135` |
| 节拍/起播等待 | DTS（换算到 `time.Duration` 后与 `time.Since(startSystem)` 比较） | `client_track.go:36-49` |
| 绝对时间外推 | DTS | `client_time_conv_*.go` 的 `getNTP` |
| 呈现 | PTS（回调参数） | `client_track.go:52`、`client.go:224-271` |
| 丢弃早于引导起点的样本 | **PTS**（`pts < 0`） | `client_track.go:30-33` |

fMP4 的样本级递推（`client_track_processor_fmp4.go:114-135`，节选）：

```go
dts := entry.dts                                // = convert(BaseTime, ClockRate)
for _, sample := range entry.partTrack.Samples {
    pts := dts + int64(sample.PTSOffset)        // trun composition offset
    var ntp *time.Time
    if entry.ntp != nil {
        ntp = new(entry.ntp.Add(timestampToDuration(dts-entry.dts, t.track.track.ClockRate)))
    }
    err = t.track.handleData(errGroupCtx, pts, dts, ntp, data)
    dts += int64(sample.Duration)
}
```

MPEG-TS 的 PTS/DTS 来自 PES 头（mediacommon `reader.go:611-630`）：`PTSDTSIndicatorBothPresent`
时两者分开，否则 `dts = pts`；AAC 的 `OnDataMPEG4Audio` 回调本身只给 PTS，封装层把
`pts, pts` 一起传入（`client_stream_processor_mpegts.go:208-211`）。

一个**容易被忽略的行为**：丢弃判据是 PTS 而非 DTS（`client_track.go:31`）。因此当首个
GOP 存在 B 帧、`PTSOffset > 0` 时，某帧可能 `PTS ≥ 0` 但 `DTS < 0`，客户端**不会丢弃**它，
而是以负 DTS 投递（此时 `dtsDuration < 0`，不触发等待）。这是刻意保留的"呈现优先"语义，
但下游需要知道 DTS 可能为负。

### 1.6 分段连续切换后，音画偏移由哪一层保持

| 状态 | 载体 | 何时建立 | 是否跨分段保留 | 是否跨 rendition 共享 |
|---|---|---|---|---|
| 33 位展开状态 | `clientTimeConvMPEGTS.td`（`prev`/`overall`） | 首个引导样本 | 是 | 是（同一对象） |
| 媒体时间原点 | `startDTS` / `leadingBaseTime`+`leadingTimeScale` | 首个引导样本 / 首个 part | 是（不再更新） | 是 |
| 时钟单位换算 | `convert(v, clockRate)` | 每次调用时按参数 | 是（参数来自 init，固定） | 是（分母是引导轨 timescale） |
| 起播墙钟锚点 | `clientTrack.startSystem` | `setTimeConv` 一次设置所有轨 | 是 | 是（所有轨同一时刻） |
| 绝对时间锚点 | `ntpValue`/`ntpTimestamp` | 每个带 PDT 的分段可重设 | 是（可更新） | 是（仅 leading 写入） |
| 播放列表 `EXTINF`/PDT | `segmentData.dateTime` | 每次下载分段 | 否（用完即弃） | 否 |

结论：**分段切换本身不参与音画偏移的保持**。保持完全依赖"展开器 + 固定原点 + 共享
`startSystem`"这三项。播放列表只影响绝对时间；`EXTINF` 时长不参与任何换算。

节拍的实现（`client_track.go:35-49`，节选）：

```go
elapsed := time.Since(t.startSystem)
dtsDuration := timestampToDuration(dts, t.track.ClockRate)
if dtsDuration > elapsed {
    diff := dtsDuration - elapsed
    if diff > clientMaxDTSSystemDiff {           // 10s, client.go:25
        return fmt.Errorf("difference between DTS and system time is too big")
    }
    select {
    case <-time.After(diff):
    case <-errGroupCtx.Done():
        return fmt.Errorf("terminated")
    }
}
```

它是**单轨、只早不晚**的节拍：任何轨道只要 DTS 超前于共享起点就等待，落后则立即投递；
不存在跨轨追赶或重采样。音画相位之所以正确，是因为所有轨都把同一个 `startSystem` 与
同一个"相对引导原点的 DTS"配对。

---

## 2. 回绕、时间单位换算与不连续

### 2.1 MPEG-TS：33 位回绕是"数值"事件

`mpegts.TimeDecoder`（mediacommon `pkg/formats/mpegts/time_decoder.go`）：

```go
const (
    maximum           = 0x1FFFFFFFF // 33 bits
    negativeThreshold = 0x1FFFFFFFF / 2
)
func (d *TimeDecoder) Decode(ts int64) int64 {
    if !d.initialized { d.initialized = true; d.overall = ts; d.prev = ts }
    diff := (ts - d.prev) & maximum
    if diff > negativeThreshold {          // 视作后退
        diff = (d.prev - ts) & maximum
        d.prev = ts; d.overall -= diff
    } else {                                // 视作前进（含正常回绕）
        d.prev = ts; d.overall += diff
    }
    return d.overall
}
```

- 90kHz 下 2³³ tick ≈ **26.5 小时**才回绕一次；33 位值从 `0x1FFFFFFFF` 跳到 0 时，
  `(0 − prev) & max` 得到一个小正数，展开器**继续累加**，因此正常回绕对上层不可见。
- 判定阈值是**半量程 ≈ 13.25 小时**：任何单步后退幅度小于半量程的跳变被当作"真实后退"，
  大于半量程的后退被当作"前进回绕"。这是把"数值回绕"与"小幅真实倒退"区分开的唯一依据；
  它**无法**区分"恰好跨越回绕点的真实重置"与"正常回绕"。
- 该解码器在客户端是**每个流一个实例、被所有轨道共用**（`client_time_conv_mpegts.go:15,34`）。
  这成立的前提是复用流内所有 PID 共用一条 90kHz 时间轴；样本文本交织导致的 `prev` 在小范围内
  来回跳动被正确累计。若某轨时间轴系统性偏离超过 13.25 小时，或两轨在不同时刻回绕，单个
  `prev` 会拼接错误。
- 测试覆盖：mediacommon 自带 `time_decoder_test.go` 覆盖首值、负差、连续回绕、回绕后回退；
  但 gohlslib 侧**只有一个**接近回绕的用例（`client_test.go:2012-2015`：PTS=90000、
  DTS=8589844592 = `0x1FFFFFFFF-90000+1`，即回绕点前 1s），
  且该用例只断言后续错误 `"next segment not found or not ready yet"`（`client_test.go:2127-2128`），
  **没有断言 pts/dts 数值**。即"首个样本就处于回绕边缘"的行为在客户端层是**未被直接验证**的。

### 2.2 fMP4：没有周期回绕，只有单位换算

fMP4 的时间由两部分给出：

- `tfdt.BaseMediaDecodeTime` → `PartTrack.BaseTime`，类型 `uint64`
  （mediacommon `parts.go:106`、`part_track.go:12`；写侧固定 version=1，`:41-47`）。
- 每轨 `mdhd.Timescale` → `init.Tracks[i].TimeScale`（`init.go:150`）。

因此：

- **不存在 2³³ 的周期性回绕**；`uint64` 的量程在 90kHz 下约 650 万年，回绕不是现实问题。
- 跨轨的"数值差异"来自**时间单位不同**：90000 vs 44100 vs 48000。客户端只在原点处做一次
  `leadingBaseTime·clockRate/leadingTimeScale` 的整除换算（`client_time_conv_fmp4.go:27`），
  每一样本的 DTS 仍以本轨单位保存，PTS 用 `PTSOffset`（同样是本轨单位）相加。
- 绝对时间换算也是"先换单位、再求差、再除时钟"
  （`client_time_conv_fmp4.go:63-66`），截断不超过 1 tick。
- 因此"数值回绕"在 fMP4 中基本不构成独立风险；**真正的风险是原点或时间轴的不连续**。

补充边界（待补证据）：`BaseTime` 以 `uint64` 读入后转 `int64`（`client_stream_processor_fmp4.go:238`），
若 `BaseTime > 2⁶³` 会变为负值，`multiplyAndDivide` 可能溢出。真实流几乎不可能达到该量级，
但代码未做校验证。另 `tfdt` version 0/1 的读取由 `abema/go-mp4` 的
`GetBaseMediaDecodeTime()` 完成（`parts.go:106`），该依赖源码不在本仓库，属外部证据。

### 2.3 真实时间不连续 vs 数值回绕

| 判别对象 | 表现形式 | 现有处理 | 结果 |
|---|---|---|---|
| MPEG-TS 正常回绕（数值） | 33 位值 0x1FFFFFFFF → 0 | `TimeDecoder` 半量程判正负后累加 | 展开值连续，上层无感 |
| MPEG-TS 小幅后退（真实重排/抖动） | 值减少，幅度 < 半量程 | 展开器做负差累计 | 值变负或回退，仍按真实后退处理 |
| MPEG-TS 真实重置（新节目/编码器重启） | 值突然归零或跳到无关值 | **仍按展开器规则处理**（无 discontinuity 支持） | 归零视 `prev` 所在半量程而分两种结果（见 §2.4）；大跳 → 触发 10s 致命检查 |
| MPEG-TS 后退幅度 > 半量程 | 值突然大幅减少 | 被误判为"前进回绕" | 展开值突然增大，可能触发 10s 致命检查 |
| fMP4 真实重置 | `BaseTime` 归零或换成新时间轴 | 只减去旧的 `leadingBaseTime` | 归零 → 负值被丢弃；大跳 → 可能 10s 致命 |
| fMP4 跨轨单位差异 | 同刻不同数值（90000 vs 44100） | 原点换算 | 正确对齐，不视为不连续 |

关键点：**`EXT-X-DISCONTINUITY` 被解析但未被消费**。播放列表解析器会置位
`MediaSegment.Discontinuity`（`pkg/playlist/media.go:243-244`、`media_segment.go:23-24`），
但 client 侧 `clientStreamDownloader` 只转发 `seg.DateTime`（`client_stream_downloader.go:213-216`），
`segmentData` 结构里也没有 discontinuity 字段（`client_segment_queue.go:9-13`）。
全仓库对 `Discontinuity` 的引用仅限 `pkg/playlist`。因此**规范意义上的不连续对客户端不可见**。

`EXT-X-DISCONTINUITY-SEQUENCE` 同样只被解析（`pkg/playlist/media.go:186-195`），无消费者。

### 2.4 归零 / 引导基准变化时，现有状态的适用范围

现有状态是二元组：

- MPEG-TS：`{td(prev, overall), startDTS}`
- fMP4：`{leadingBaseTime, leadingTimeScale}`（`leadingTimeScale` 来自 init，正常情况下不变）

**适用条件**（超出即失效）：

1. 同一客户端生命周期内，所有轨共用一条连续时间轴；
2. 引导轨原点只出现一次（首样本 / 首 part），此后不再改变；
3. 相邻样本的数值步进在展开器可判别范围内（MPEG-TS 单步后退 < 13.25h）；
4. fMP4 的 `BaseTime` 单调不减且非负；
5. 时间戳的数值范围不使 `int64`/`time.Duration` 溢出（MPEG-TS 2³³ tick ≈ 26.5h 安全；
   fMP4 近 2⁶³ 不安全）。

**失效时的具体行为**（逐条对应源码）：

展开器的分支只取决于**回退瞬间的 `prev`** 落在半量程的哪一侧（`time_decoder.go:36-46`），
与 `startDTS` 无关；因此同一"归零"在不同时刻会有两种结果：

| 场景 | 模块判定 | 结果 |
|---|---|---|
| MPEG-TS 原始时间归零，`prev < 半量程`（原始值距回绕原点 < 13.25h） | `(0 − prev) & max = 2³³ − prev > 半量程` → 判为**后退** `prev` tick，`overall` 归 0 | 换算后为 `−startDTS`（负）→ **静默丢弃**（`client_track.go:31`）。恢复需原始值重新爬升到 `startDTS`，本例 `startDTS` 接近 `2³³`，故**丢弃期长约 26.5 小时**，实际等同永久静默 |
| MPEG-TS 原始时间归零，`prev ≥ 半量程` | `(0 − prev) & max = 2³³ − prev ≤ 半量程` → 判为**正常前进回绕** | 媒体时间前跳恰好 `2³³ − prev` tick（`overall` 变为 `2³³`）。是否致命取决于前跳后 `dtsDuration − elapsed` 是否 > `clientMaxDTSSystemDiff`：前跳幅度越大、越可能致命（`client_track.go:38-42`）；前跳很小（`prev` 距 `2³³` 仅几秒）时可能被当作正常播放继续 |
| MPEG-TS 真实大幅后退（幅度 > 半量程） | `(prev − ts) & max > 半量程`，被误判为前进回绕 | 媒体时间前跳一个大幅值；通常使 `dtsDuration − elapsed > 10s` → **致命错误**（`client_track.go:38-42`） |
| MPEG-TS 引导轨切换到另一条时间轴（新节目） | 仍用旧 `startDTS` 与旧展开状态 | 恒定偏移（大小=两轴之差）；若 > 10s 则致命 |
| fMP4 `BaseTime` 归零，`leadingBaseTime > 0` | 直接减去 `leadingBaseTime·rate/scale` | 负值 → `pts < 0` → 静默丢弃；若 PTS 因 `PTSOffset` 非负则**以负 DTS 投递**（见 §1.5） |
| fMP4 `BaseTime` 前跳 > 10s | `dtsDuration − elapsed > 10s` | 致命错误 |
| fMP4 引导轨 `BaseTime` 在后续分段改变 | 不减新基准，只减首段 `leadingBaseTime` | 恒定偏移；客户端不重新初始化（`initializeTimeConv` 仅在 `trackProcessors == nil` 时调用，`client_stream_processor_fmp4.go:148-158`） |

**恢复能力**：媒体时间没有任何自动重置路径；只有绝对时间可以靠"分段带 PDT 时重新 `setNTP`"
恢复锚点（§1.4）。但被重置的样本通常已在 `pts < 0` 处被丢弃，不会到达回调，因此对调用方
表现为"静默停止"而不是"绝对时间错乱"。

### 2.5 缺失播放日期信息的影响

| 维度 | 是否依赖 PDT | 依据 | 缺失时的行为 |
|---|---|---|---|
| 媒体时间（pts/dts） | **否** | 仅由分段内时间戳与固定原点决定 | 完全正常 |
| 分段间媒体时间连续性 | 否 | 展开器 + 固定原点 | 完全正常 |
| 投递节拍 | 否 | `startSystem` 是 `time.Now()`，与 PDT 无关（`client_primary_downloader.go:154-158`） | 完全正常 |
| 绝对时间 `AbsoluteTime()` | **是** | `ntp == nil` → `lastAbsoluteTime = nil` → 返回 `(zero,false)`（`client_track.go:16-21,51`） | 返回不可用；**不会阻塞、不会报错** |

关键的"不阻塞"保证：`setLeadingNTPReceived()` 在 leading 流首次处理引导样本时**无条件**调用
（`client_stream_processor_mpegts.go:288`、`client_stream_processor_fmp4.go:168`），即使
`dateTime == nil`。因此 `getNTP` 的 `select` 不会永久等待（`client_time_conv_mpegts.go:56-60`），
只是返回 `nil`。这是媒体播放不受 PDT 缺失影响的结构性原因。

PDT 的**可获得性差异**（对绝对时间的影响）：

- MPEG-TS muxer 对**每个分段**都输出 PDT（`muxer_stream.go:443-448`），所以 MPEG-TS 客户端
  从第一段起就有绝对时间。
- fMP4 muxer **只对最后两个分段**输出 PDT（`muxer_stream.go:527-529`：`len(s.segments)-i <= 2`）。
  因此从分段 0 开始播放的 fMP4 VOD，**绝大部分时间拿不到绝对时间**，直到接近末尾；
  fMP4 直播从 `StartDistance=3` 起播时，也可能第一段没有 PDT（`client.go:91-94`）。
  LL 模式下的 preload hint 用"最后一段 PDT + 时长 + 各 part 时长"推算
  （`client_stream_downloader.go:54-71`），只要最后一段带 PDT 即可用。
- PDT 只解析到毫秒（`pkg/playlist/media.go:13-28`，格式 `2006-01-02T15:04:05.999Z07:00`，
  小数秒可省略），亚毫秒精度丢失。

**结论**：缺少播放日期信息把"媒体时间"与"绝对时间"彻底解耦——媒体播放完全不受影响，
绝对时间变为不可用；反过来，PDT 存在与否也不会改变 pts/dts 或音画偏移。

---

## 3. 保持既有正常播放行为的处理建议

### 3.1 客户端（本仓库）已有的保证

这些是源码已经兑现、无需改动的契约：

1. **单一共享时基**：每个 client 只有一个 `clientTimeConv` 对象，视频与所有音频 rendition
   共用（`client_primary_downloader.go:151-170`），混合容器被显式拒绝
   （`client_stream_processor_mpegts.go:335-338`、`client_stream_processor_fmp4.go:249-252`）。
2. **引导轨原点唯一且固定**：只在首样本/首 part 设置一次，后续分段不重设
   （`client_stream_processor_mpegts.go:235-239`、`client_stream_processor_fmp4.go:148-158`）。
3. **跨时钟频率对齐**：fMP4 用 `leadingBaseTime·clockRate/leadingTimeScale` 把原点换算到
   本轨单位（`client_time_conv_fmp4.go:26-28`），绝对时间按同样方式换算
   （`:63-66`）；MPEG-TS 全轨 90kHz。
4. **33 位正常回绕透明**：`TimeDecoder` 跨分段持续展开（`time_decoder.go:36-46`）。
5. **早于引导起点的样本被丢弃**：`pts < 0` 语义（`client_track.go:30-33`），这既是音画对齐
   机制，也是无序 rendition 的保护。
6. **PDT 缺失不阻塞**：`setLeadingNTPReceived` 无条件关闭（`client_stream_processor_mpegts.go:288`）。
7. **节拍上界**：DTS 超前超过 10s 即致命，避免无限缓冲（`client_track.go:40-42`）。
8. **分段切换顺序与背压**：MPEG-TS 用有界队列（`clientMPEGTSSampleQueueSize=100`）并在分段末
   汇总（`client_stream_processor_mpegts.go:119-136`），fMP4 用 WaitGroup 等待全部 part
   （`client_stream_processor_fmp4.go:171-197`）。

### 3.2 客户端：待补证据 / 建议的改动（默认不改变现有行为）

原则：**新增处理必须由显式信号触发，默认路径保持逐字节一致**，以免破坏既有正常播放。

| # | 问题 | 现状证据 | 建议 | 验收判据（可静态执行） |
|---|---|---|---|---|
| C1 | 不连续未处理 | `Discontinuity` 仅解析未使用（`pkg/playlist/media.go:243-244`） | 在 `segmentData` 增加 `discontinuity bool`，由下载器从 `seg.Discontinuity` 透传；默认不改变时基，仅在显式开启时重建 `timeConv` | 新增用例：带 `EXT-X-DISCONTINUITY` 的分段在默认配置下 pts/dts 与现在完全一致；开启重建后，该段首样本换算值为 0 |
| C2 | 原点归零无可观测信号 | 无检测代码 | 在 `convert` 前后检测"换算后 DTS 相对上一段的突变"，用 `OnDecodeError` 上报（非致命） | 用例：构造时间戳归零的分段，断言收到一次非致命错误，且默认行为仍为丢弃 |
| C3 | 首个样本即回绕未验证 | `client_test.go:2012-2015` 无数值断言 | 增加针对该分段的 pts/dts 断言 | 用例：断言首样本 `dts==0`，次样本 `dts==3000` |
| C4 | PDT 缺失时 `AbsoluteTime` 恒 false 的覆盖不足 | 现有测试都带 PDT | 增加"首段无 PDT、后续段有 PDT"的用例 | 断言首段回调 `AbsoluteTime()==false`，出现 PDT 后为 `true` 且值正确 |
| C5 | fMP4 `BaseTime` 转 `int64` 无溢出校验 | `client_stream_processor_fmp4.go:238` | 增加上界检查并报错 | 用例：`BaseTime` 接近 2⁶³ 时返回明确错误而非溢出 |
| C6 | `pts<0` 丢弃只按 PTS，负 DTS 可透传 | `client_track.go:31` | 保持现状，但在文档/注释中明确"DTS 可为负" | 用例：`PTSOffset>0` 的首帧，断言被投递且 `dts<0` |
| C7 | 音频 rendition 用独立时间轴时产生恒定偏移 | 无检测 | 增加可选的一致性检查（比较各流首段相对位置与实际 PDT 差） | 用例：音频 `BaseTime=0`、视频 6s，断言产生 6s 偏移（记录该契约） |
| C8 | astits 的 33 位编码、go-mp4 的 tfdt v0/v1 读取 | 依赖源码不在仓库 | 以依赖版本锁定 + 注释说明 | 记录 `go-astits v1.17.0`、`abema/go-mp4 v1.7.4` 的对应行为为外部证据 |

### 3.3 上游媒体生产者的责任边界

客户端只能保证"**收到的时间戳是可解释的**"，无法重建生产者没有表达的信息。分工如下：

| 责任 | 生产者必须保证 | 本仓库 muxer 的现状 | 客户端的可观测后果 |
|---|---|---|---|
| 时间轴统一 | 同一会话内所有 rendition 共用一条媒体时间轴，`tfdt` 原点可比 | fMP4 用同一 `nextDTS/nextNTP` 轮换所有流（`muxer.go:650-669`），音频跟随引导流轮换 | 违约时出现恒定音画偏移，客户端不会报错 |
| 原点稳定 | 会话内不重置时间戳原点 | muxer 不重置；分段轮换沿用同一时间轴 | 违约时静默丢弃或 10s 致命 |
| 回绕而非重置 | MPEG-TS 到 2³³ 边界应继续按模回绕，不得归零 | mediacommon 把 PTS/DTS 原样交给 astits（`mediacommon/pkg/formats/mpegts/writer.go:377-383`），33 位取模由 `go-astits v1.17.0` 完成（外部证据） | 违约时见 §2.4 |
| fMP4 BaseTime 非负 | 用 `fmp4StartDTS=10s` 预留负 DTS 空间（`muxer.go:23`、`muxer_segmenter.go:514-521`） | 负 DTS 返回 `"sample timestamp is impossible to handle"`（`:519-521`，测试 `muxer_test.go:2352-2380`） | 违约时 muxer 直接报错 |
| 时长与 DTS 单调 | 样本 DTS 回退时把前一样本时长置 0（`muxer_segmenter.go:526-534`） | 已实现（测试 `TestMuxerFMP4NegativeDTSDiff`，`muxer_test.go:2382-2462`） | — |
| MPEG-TS AAC PTS 一致 | AAC 按 AU 数递推，源 PTS 漂移 > 500ms 才重同步（`muxer_segment_mpegts.go:124-145`，`muxer.go:26`） | 已实现（测试 `muxer_test.go:1159-1284`） | 违约时 muxer 主动纠正 |
| 时钟频率 | 视频 90000、AAC=采样率、Opus=48000（`muxer.go:246-249`、`codecRequiredClockRate:116`） | 非法值启动即拒绝（`muxer_test.go:1094-1157`） | — |
| 不连续声明 | 真实重置时必须输出 `EXT-X-DISCONTINUITY`（或新 init/新 URI），并保证 PDT 与该点一致 | muxer 不产生 discontinuity（编码路径无双时间轴） | 客户端目前不消费该标记（C1） |
| PDT 密度 | 绝对时间需求方要求每段或高频输出 PDT | MPEG-TS 每段输出（`muxer_stream.go:444`）；fMP4 仅末两段（`:527-529`） | fMP4 早期分段无绝对时间 |

### 3.4 不破坏既有行为的边界条件（回归判据）

以下判据可直接作为"未改变正常播放"的验收标准：

1. `TestClient` 全部子用例（`client_test.go:138-1224`）的 pts/dts/NTP 断言逐项不变，特别是
   `:1125`（`dts==0`）、`:1126`（`pts==180000`）、`:1137/:1138`、`:1166/:1170`。
2. 音频 rendition 的 PDT（2014 年）仍被忽略，绝对时间仍取引导流（`:1168-1170`）。
3. `TestClientByteRange`（`:2228-2319`）中跨分段 `dts==0/90000` 不变。
4. `TestMuxerFMP4NegativeInitialDTS`、`TestMuxerFMP4NegativeDTSDiff`、
   `TestMuxerMPEGTSAACResync*` 全部保持通过。
5. 新增的 discontinuity/重置处理在默认配置下不改变任何既有输出（可用"默认关闭"断言）。

---

## 附录 A：关键代码索引

| 主题 | 文件:行 |
|---|---|
| 客户端常量（10s、队列上限） | `client.go:21-29` |
| `AbsoluteTime` | `client.go:273-278`、`client_track.go:16-21` |
| 节拍与 `pts<0` 丢弃 | `client_track.go:23-53` |
| MPEG-TS 时基 | `client_time_conv_mpegts.go:11-72` |
| fMP4 时基 | `client_time_conv_fmp4.go:9-69` |
| MPEG-TS 引导轨选择 | `client_stream_processor_mpegts.go:17-34` |
| MPEG-TS 样本排队/换算/锚点 | `client_stream_processor_mpegts.go:223-299` |
| MPEG-TS 时基初始化与跨流共享 | `client_stream_processor_mpegts.go:321-341` |
| fMP4 引导轨/单位 | `client_stream_processor_fmp4.go:15-45, 77-104` |
| fMP4 分段处理/换算/锚点 | `client_stream_processor_fmp4.go:136-200` |
| fMP4 时基初始化/跨流共享 | `client_stream_processor_fmp4.go:229-255` |
| fMP4 样本递推（DTS/PTS/NTP） | `client_track_processor_fmp4.go:111-138` |
| 共享 `startSystem` | `client_primary_downloader.go:151-170` |
| 音频 rendition 建立 | `client_primary_downloader.go:216-293` |
| PDT 透传/LL 推算 | `client_stream_downloader.go:54-71, 179-228` |
| 33 位展开 | mediacommon `pkg/formats/mpegts/time_decoder.go:3-49` |
| PES PTS/DTS 读取 | mediacommon `pkg/formats/mpegts/reader.go:611-630` |
| tfdt / trun / timescale 解析 | mediacommon `pkg/formats/fmp4/parts.go:106,155-159`、`part_track.go:41-47`、`init.go:150` |
| 时间换算辅助 | `muxer_segmenter.go:19-37` |
| muxer fMP4 起播 DTS 偏移 | `muxer.go:23`、`muxer_segmenter.go:514-534` |
| muxer AAC PTS 重同步 | `muxer_segment_mpegts.go:124-145` |
| muxer PDT 输出密度 | `muxer_stream.go:443-448`（MPEG-TS）、`:527-529`（fMP4） |
| 播放列表 discontinuity/PDT 解析 | `pkg/playlist/media.go:243-244, 249-256`、`media_segment.go:23-30` |

## 附录 B：测试覆盖矩阵（与时间相关的断言）

| 行为 | 测试 | 覆盖情况 |
|---|---|---|
| MPEG-TS 原点 + 跨分段连续 | `client_test.go:203-246, 1122-1171` | 覆盖（含 +1s/−1s 首帧） |
| fMP4 跨轨单位换算 + NTP | `client_test.go:346-410, 1122-1191` | 覆盖（90000/44100） |
| fMP4 多音频共享基准 | `client_test.go:583-800, 1176-1191` | 覆盖（PDT 不同仍取视频锚点） |
| fMP4 多音频独立 PLN 时的偏移 | `client_test.go:417-581` | 覆盖 |
| LL part 时基 | `client_test.go:804-946, 1122-1171` | 覆盖 |
| ByteRange 跨分段 | `client_test.go:2228-2319` | 覆盖 |
| MPEG-TS 首样本近回绕 | `client_test.go:2011-2021` | **未断言数值** |
| 真实不连续 / `EXT-X-DISCONTINUITY` | 无 | **未覆盖** |
| 原点归零 / 引导基变化 | 无 | **未覆盖** |
| 无 PDT 时 `AbsoluteTime=false` | 无 | **未覆盖** |
| fMP4 早期分段无 PDT | 无 | **未覆盖** |
| 负 DTS + 正 PTS 首帧 | 无 | **未覆盖** |
| muxer fMP4 负 DTS | `muxer_test.go:2352-2462` | 覆盖 |
| muxer AAC PTS 重同步 | `muxer_test.go:1159-1284` | 覆盖 |
| muxer 时钟频率校验 | `muxer_test.go:1094-1157` | 覆盖 |
