# HLS 主播放列表 → 媒体读取：变体与伴随音频组的选择策略分析

- 仓库：`bluenviron/gohlslib` v2（本工作区，HEAD `b771acf`）
- 范围：从 `Client.Start()` 读取主播放列表（Multivariant）到 `OnTracks` 交付轨道、再到采样回调的**实际代码路径**
- 方法：**纯静态阅读**。不连接媒体服务、不启动容器；本机 Go 工具链不可用（`GOROOT` 缺失 / 离线无法下载 `go1.26.0`），因此**未执行测试**，下文"已有测试支持"均指**阅读测试源码**得出的覆盖关系，而非运行结果
- 依赖边界：`mediacommon` 未 vendor，`fmp4.Init.Unmarshal` 对未知 codec 的具体行为未纳入验证（见 §6）

---

## 0. 结论摘要

| 问题 | 结论 |
| --- | --- |
| 多变体 + 伴随音频组，选哪些媒体来源 | 只在 **`Start()` 时选一次**：按 `CODECS` 字符串白名单过滤，取 `BANDWIDTH` 最大者；再**把该变体 `AUDIO` 组里所有带 `URI` 的 rendition 全部启动**（不是选一条） |
| 声明支持 vs 实际可读 | `CODECS` 仅用于**过滤候选变体**，**从不**用于决定实际轨道；实际轨道来自 init 段 / PMT。两者可完全不一致 |
| 外置音轨 vs 内嵌音轨 | 内嵌音轨由主流自己产出；外置 rendition 单独启动。**两者同时存在就会重复交付音频**。`URI` 缺失的 rendition 被**静默跳过**，假定音频内嵌 |
| 同组多候选 | 无优先级：`DEFAULT` / `AUTOSELECT` **完全不参与选择**，全部入选并各自成为一路轨道 |
| 组引用缺失 | 硬错误，整个 client 失败（`no playlist with Group ID ... found`） |
| 某一路失败 | **无降级**：交付轨道前失败 → 全部失败；交付后失败 → 整个 client 立即终止。EOS 需**全体**到齐 |
| 带宽变化场景 | **不支持动态切换**。主播放列表只下载一次，选中的变体、音频组、时间基准终身固定；`StartDistance`/`MaxDistance` 只影响直播起点与"落后"判定 |

---

## 1. 调用链总览

```
Client.Start()                                   client.go:137
  └─ run() → runInner()                          client.go:280,285
       └─ clientPrimaryDownloader.run()          client_primary_downloader.go:179
            ├─ downloadPlaylist(主播放列表)        :182   ← 全生命周期仅此一次
            ├─ pl.(*playlist.Multivariant):
            │    ├─ pickLeadingPlaylist(Variants) :217  ← 选择策略
            │    ├─ 启动 leading stream           :228
            │    └─ getRenditionsByGroup(Audio)   :252  ← 组解析
            │         └─ 每个 URI≠nil 的成员启动一路 stream :269
            ├─ 【汇总】等待每路 stream 的 chTracks :301-313
            ├─ parent.setTracks(tracks)           :319 → Client.OnTracks
            ├─ 【放行】startStreaming(allTracks)   :324
            └─ 【终止】等待 len(streams) 个处理器消息 :333
       └─ errgroup.Wait()                         client.go:317
```

每一路 `clientStreamDownloader` 内部再启动一个处理器：

- 有 `EXT-X-MAP` → `clientStreamProcessorFMP4`（`client_stream_downloader.go:122-145`）
- 否则 → `clientStreamProcessorMPEGTS`（`client_stream_downloader.go:146-159`）

两条路径的职责分工在本报告中反复出现，先固定下来：

| 角色 | 承担的事 | 关键位置 |
| --- | --- | --- |
| `pickLeadingPlaylist` | 变体选择（唯一决策点） | `client_primary_downloader.go:84-105` |
| `getRenditionsByGroup` | 音频组成员枚举 | `client_primary_downloader.go:107-120` |
| 处理器 `run` | 从 init/PMT 推导**实际**轨道并上报 | `client_stream_processor_fmp4.go:67-134`、`client_stream_processor_mpegts.go:145-190` |
| 汇总循环 | 把 N 路轨道拼成**一个** `[]*Track`，保证顺序 | `client_primary_downloader.go:299-322` |
| `onProcessorError` / `errgroup` | 决定局部错误是否致命 | `client_primary_downloader.go:172-177,331-345` |

---

## 2. 场景一：多个视频变体声明不同编解码并引用伴随音频组

### 2.1 选择算法（逐行）

```go
// client_primary_downloader.go:19
func checkSupport(codecs []string) bool {
	for _, codec := range codecs {
		if !strings.HasPrefix(codec, "avc1.") &&
			!strings.HasPrefix(codec, "hvc1.") &&
			!strings.HasPrefix(codec, "hev1.") &&
			!strings.HasPrefix(codec, "mp4a.") &&
			codec != "opus" {
			return false          // 任意一项不支持 → 整个变体淘汰
		}
	}
	return true
}

// client_primary_downloader.go:84
for _, v := range variants {
	if !checkSupport(v.Codecs) { continue }
	candidates = append(candidates, v)
}
...
// pick the variant with the greatest bandwidth
if leadingPlaylist == nil || v.Bandwidth > leadingPlaylist.Bandwidth { ... }
```

由此可以确定四条硬性规则：

1. **全有或全无**：`CODECS` 中只要有一项不在白名单，变体整体淘汰。`CODECS="avc1.42c028,ec-3"` 会被丢弃，哪怕客户端**根本不需要解码 `ec-3`**（它本可以从 `AUDIO` 组取 AAC）。
2. **白名单不完整**：`av01.*`、`vp09.*`、`flac` 均不在白名单，但客户端**在单播放列表模式下是支持它们的**（`fromFMP4` 覆盖 AV1/VP9/FLAC，`client_test.go:1225-1577` `TestClientCodecs` 单播放列表全通过）。这是"声明支持 ≠ 实际可读"的第一个断裂点：**能被解码的变体在多变体模式下会被直接淘汰**。

   这一点在同仓库内即可自证：muxer 生成 `CODECS` 时用 `codecparams.Marshal`（`muxer_stream.go:183-187`），AV1 输出 `av01.0.08M.08.0.110.01.01.01.0`（`pkg/codecparams/marshal.go:129-176`，断言见 `muxer_test.go:1006-1007`）。也就是说 **gohlslib 自己产出的 AV1/VP9/FLAC 主播放列表，交给 gohlslib 自己的 client 会因 `checkSupport` 失败而报 `no variants with supported codecs found`**。muxer 侧的 codec 字符串测试（`TestMuxerCodecs`）与 client 侧的解码测试（`TestClientCodecs`）各自通过，但从未串联——两个测试套件之间缺少一条"muxer 输出 → client 输入"的闭环用例。
3. **只看 `BANDWIDTH`**：`AVERAGE-BANDWIDTH`、`RESOLUTION`、`FRAME-RATE` 都不参与；比较用严格 `>`，**并列时取播放列表中靠前者**（顺序敏感）。
4. **只看声明，不看能否解析组**：`pickLeadingPlaylist` 不检查该变体的 `AUDIO` 组是否存在。于是"带宽最大的变体引用了不存在的组"会先被选中，随后在 `:253-255` 才炸掉。

另外两个解析层的细节会直接改变选择结果：

- `CODECS` 的拆分在 `multivariant_variant.go:75` 是裸 `strings.Split(val, ",")`，**不 trim 空白**。`CODECS="avc1.42c028, mp4a.40.2"`（逗号后有空格）会得到 `" mp4a.40.2"`，`HasPrefix("mp4a.")` 失败 → 整个变体被淘汰。合法但不规范的写法即可改变选路。
- `CODECS` 缺失时 `v.Codecs` 为 `nil`，`checkSupport(nil)` **返回 `true`**（空循环）。虽然 RFC 8216 要求该属性必须存在，实现上会放行"没有任何声明"的变体，于是选择完全退化为"最大 `BANDWIDTH`"。

### 2.2 声明支持与实际轨道可读的区别

三层彼此独立，代码从不交叉校验：

| 层 | 来源 | 代码 | 对结果的影响 |
| --- | --- | --- | --- |
| 声明层 | `EXT-X-STREAM-INF:CODECS` | `client_primary_downloader.go:19-30` | 仅决定**候选变体是否入围** |
| 封装层 | 媒体播放列表有没有 `EXT-X-MAP` | `client_stream_downloader.go:122` | 决定走 fMP4 还是 MPEG-TS 处理器 |
| 实际层 | init 段 `tracks` / PMT 元素流 | `client_stream_processor_fmp4.go:68,81-104`；`client_stream_processor_mpegts.go:158-180` | **唯一**决定 `OnTracks` 看到什么 |

- fMP4：轨道完全由 `fmp4.Init.Tracks` 决定，`fromFMP4` 逐项转换（`from_to_fmp4.go:9-56`）；`CODECS` 一个字节都不读。
- MPEG-TS：只保留 `H264` / `MPEG4Audio` / `KLV` 三类（`client_stream_processor_mpegts.go:160-165`），其余元素流**静默丢弃**；`TestClientSkipUnsupportedTracks`（`client_test.go:1829-1958`）验证了 DIRAC 流被丢弃后只剩一条 H264。

因此三种错配都成立（均为静态可推，但见 §5 的测试缺口）：

- 声明只写 `avc1.*`，媒体播放列表里其实只有音频 → 实际交付纯音频轨道，不报错。
- 声明 `avc1.*,mp4a.*`，但 `EXT-X-MAP` 指向的 init 段只有一条视频轨道 → 交付视频 + 外置 rendition 音频；声明中的 `mp4a` 从未兑现。
- 声明能通过白名单，init 段里却是 `ec-3`/`ac-3` → MPEG-TS 路径静默丢弃；fMP4 路径下 `fromFMP4` 的 `switch` 无匹配分支会落到函数末尾的 `return nil`（`from_to_fmp4.go:9-56`），而 `client_track_processor_fmp4.initialize` 的 `switch` 同样无 `default`，此时 `decodePayload` 保持 `nil`（`client_track_processor_fmp4.go:35-93`），后续 `t.decodePayload(sample)`（`:117`）缺乏判空。**该分支能否到达取决于 `mediacommon` 的 `Init.Unmarshal` 是否放行未知 codec，本静态阅读未验证**（§6）。

### 2.3 外置音轨与内嵌音轨如何影响组合结果

音频组解析只有一处：

```go
// client_primary_downloader.go:251
if leadingPlaylist.Audio != "" {
	audioPlaylists := getRenditionsByGroup(plt.Renditions, leadingPlaylist.Audio)
	if audioPlaylists == nil {
		return fmt.Errorf("no playlist with Group ID \"%s\" found", ...)
	}
	for _, pl := range audioPlaylists {
		// stream data already included in the leading playlist
		if pl.URI == nil { continue }          // :259-261
		...
		renditionStream := &clientStreamDownloader{ isLeading: false, rendition: pl, ... }
	}
}
```

要点：

1. **"选一个变体" ≠ "选定一组媒体"**。选中变体只固定了**视频侧**；音频侧是**整组订阅**——`DEFAULT=YES`、`AUTOSELECT=YES` 都不参与，`NAME="English"` 和 `NAME="German"` 会**同时**成为两条轨道。
   - `TestClient` 的 `fmp4_multiplaylist / video+multiaudio`（`client_test.go:583-593`）明确断言 `tracks[1]={Name:"English"}`、`tracks[2]={Name:"German", IsDefault:true}`（`:1082-1119`），两路数据都送达（`:1176-1211`）。
   - `TestClientLifecycleStaggeredRenditionEOS`（`client_test.go:2731-2842`）进一步证明 English 与 Spanish 都交付，且**EOS 要等最慢的 Spanish**。
2. **`URI == nil` 是"内嵌"约定，不是校验**。跳过该成员即假设音频已在主流里；若服务器实际没内嵌，客户端**不会**报错，调用方只会少一条音轨。该约定由 muxer 生成端印证：`muxer_stream.go:257-264` 对 leading 音轨省略 `URI`，对非 leading 音轨写入 `URI`。
3. **内嵌 + 外置并存 = 重复音频**。若被选变体的媒体播放列表本身含 AAC，同时组里又有带 `URI` 的 AAC，两路都会进来，`OnTracks` 出现两条同 codec 音频轨。**无去重、无 codec/语言比对**（代码中不存在类似逻辑）。当前测试夹具的主流恰好都是纯视频（`init_video.mp4` 只含 1 条轨道，`client_test.go:491-503`），所以从未触发。
4. **`AUDIO` 组按 `GroupID` 过滤，不校验 `Type`**。`getRenditionsByGroup`（`:107-120`）只比较字符串。若同一 `GROUP-ID` 同时被 `TYPE=AUDIO` 与 `TYPE=VIDEO/SUBTITLES` 使用（规范上 group 按 TYPE 作用域，现实里可能撞名），非音频 rendition 也会被当作音频流启动。
5. **`SUBTITLES` / `CLOSED-CAPTIONS` / `VIDEO` 属性完全没有代码路径**。变体声明了字幕组，库不会读取，也不会告知调用方。

### 2.4 具体推演（声明支持 ≠ 可读）

```m3u8
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",DEFAULT=YES,URI="en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="German",URI="de.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="ec3",NAME="English EC3",URI="en-ec3.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=8000000,CODECS="hvc1.1.6.L120.90,ec-3",AUDIO="ec3"
8m.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS="avc1.42c028,mp4a.40.2",AUDIO="aac"
4m.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="av01.0.08M.08",AUDIO="aac"
2m.m3u8
```

静态推演结果：

| 变体 | `checkSupport` | 是否成为 leading |
| --- | --- | --- |
| 8m（`hvc1` + `ec-3`） | `ec-3` 不在白名单 → `false` | 否 |
| 4m（`avc1` + `mp4a`） | 全通过 | **是（候选中最高的 BANDWIDTH）** |
| 2m（AV1） | `av01.` 不匹配 → `false` | 否 |

最终启动的媒体来源：

1. `4m.m3u8`（leading，`isLeading: true`）；
2. `en.m3u8`、`de.m3u8`（`aac` 组两成员，各自一路）；
3. `en-ec3.m3u8` **不会**被访问（`ec3` 组未被任何入选变体引用）；
4. `2m.m3u8` **不会**被访问，尽管 AV1 在单播放列表模式下可正常解码。

交付轨道顺序 = `[4m 的实际轨道..., English, German]`，与 `streams` 追加顺序一致（`:301-305`）。8 Mbps 的 HEVC 与 2 Mbps 的 AV1 都被"声明"这一层挡掉了——选择结果与真实可解码能力、与真实可用带宽都没有直接关系。

---

## 3. 场景二：同组多候选、组引用缺失、单路失败

### 3.1 三件事各自的责任边界

用户问题要求区分"选择优先级 / 轨道汇总 / 错误传播"。代码里这三者确实分层，但都收敛到一个"全或无"的判定：

| 阶段 | 负责什么 | 具体行为 | 失败表现 |
| --- | --- | --- | --- |
| **选择优先级** | 决定启动哪几路 stream、按什么顺序 | 变体：支持性白名单 + 最大 `BANDWIDTH`；组内：**全部入选**，无 `DEFAULT`/`AUTOSELECT` 排序；`URI==nil` 跳过 | 组不存在 → 硬错误；`URI` 缺失 → 静默跳过 |
| **轨道汇总** | 把各路轨道合成一个 `[]*Track` | 逐路从 `chTracks` 收，按 `streams` 顺序 `append`；**全部到齐后**才调用一次 `OnTracks` | 任一路不交轨道 → 汇总阻塞（永久挂起）或直接失败 |
| **错误传播** | 决定"局部错误是否致命" | 段的下载/解码错误经 `segmentQueue` → `onProcessorError` → `chProcessorError`；处理器直接返回的错误交给 `errgroup` 首错 | 任一非 `ErrClientEOS` 错误 → **整个 client 终止**；EOS 需全体到齐 |

### 3.2 同组多候选：没有"优先级"，只有"全取"

`getRenditionsByGroup` 是纯字符串过滤，返回**播放列表顺序**的全部成员（`:107-120`）。因此：

- 两条 AAC 候选（不同语言）→ 两条轨道，调用方自己按 `Name`/`Language`/`IsDefault` 取舍。
- 同一语言的两条候选（例如 `LOW`/`HIGH` 码率）→ 两条轨道**同时**下载、同时回调，**不是"择一"**。带宽与 `CHANNELS` 都不参与。
- 组内成员的播放列表若指向同一个 URL（常见于多个 `EXT-X-MEDIA` 复用同一流）→ **无去重**，会并行下载同一份媒体两次并产生两条同 codec 轨道。
- `IsDefault` 只被"抄写"到 `Track`，不参与任何决策；且只在 fMP4 非 leading 路径填充（`client_stream_processor_fmp4.go:85-102`）。MPEG-TS rendition 处理器**根本没有** `rendition` 字段（`client_stream_processor_mpegts.go:44-60`），因此 MPEG-TS 外置音轨的 `Name`/`Language`/`IsDefault` 恒为空。
- **内嵌音轨拿不到任何元数据**：同一断言块里，`mpegts / video+audio` 的音频轨（内嵌在主流中）期望 `Name:""`、`Language:""`、`IsDefault:false`；而 `fmp4_multiplaylist / video+audio` 的音频轨（外置 rendition）期望 `Name:"English"`、`Language:"en"`、`IsDefault:true`（`client_test.go:1046-1080`）。也就是说，**只要音频是内嵌的，调用方就无法从 API 判断它的语言或默认性**——因为 leading 处理器把这三个字段硬编码为空（`client_stream_processor_fmp4.go:85-102` 的 `if !p.isLeading` 分支）。

### 3.3 缺失组引用：选择阶段的硬失败

```go
// client_primary_downloader.go:252-255
audioPlaylists := getRenditionsByGroup(plt.Renditions, leadingPlaylist.Audio)
if audioPlaylists == nil {
	return fmt.Errorf("no playlist with Group ID \"%s\" found", leadingPlaylist.Audio)
}
```

- 这是**组解析**的责任，不是汇总或传播的责任：`OnTracks` 从未触发，所有已启动的 stream 随 `errgroup` 取消。
- 判定发生在 `pickLeadingPlaylist` **之后**，所以"带宽最高但组缺失"的变体会先被选中再失败——不会回退到次高变体。代码里没有任何变体回退循环。
- 空组（`AUDIO` 属性存在但播放列表里确实没有该组）与拼写错误同价。**该错误字符串在现有测试中无任何覆盖**（`grep` 仅命中源码）。

### 3.4 交付轨道前失败：谁也救不回来

```go
// client_primary_downloader.go:299-313
for _, stream := range streams {
	select {
	case streamTracks := <-stream.chTracks:
		tracks = append(tracks, streamTracks...)
	case err = <-d.chProcessorError:
		// a stream failed before providing its tracks
		return err
	...
	}
}
```

- 这是**全局屏障**：任一路在交出轨道前失败，已就绪的其他轨道**全部丢弃**，整个 `Client` 以该错误结束。视频完好也无济于事。
- `TestClientLifecycleErrorBeforeTracks`（`client_test.go:2896-2954`）覆盖了该语义：`renditions` 子用例中视频段 404、音频段挂起，断言 `Wait2` 返回非 EOS 错误且含 `404`。
- 若某一路**既不失败也不交付**（媒体播放列表请求永久挂起），汇总循环会无限期阻塞，`OnTracks` 永不触发。**没有超时**：唯一出口是 `Client.Close()` / context 取消（`TestClientLifecycleCloseBlockedHTTP` 的 `descendant` 用例只验证了"Close 能解开"，未验证"能否自动超时"）。
- `select` 的两个分支同时就绪时由运行时**随机**选择（`:302-308`），但这对可观察结果没有影响：任一路错误最终都会让整个循环 `return err`，已收集的 `tracks` 全部作废。

### 3.5 交付轨道后失败：仍然整体终止

```go
// client_primary_downloader.go:331-345
// a real error in any stream terminates the client immediately,
// while the end of stream is reported only once all streams have ended.
for range streams {
	select {
	case err = <-d.chProcessorError:
		if !errors.Is(err, ErrClientEOS) { return err }
	case <-errGroupCtx.Done():
		return fmt.Errorf("terminated")
	}
}
return ErrClientEOS
```

- **无降级路径**：没有"移除失败轨道、继续呈现剩余轨道"的分支，也没有向调用方通知"某轨已失效"的回调。健康轨道仍在回调，但 `Wait2()` 很快就会返回错误，`done` 关闭。
- `TestClientLifecycleRenditionErrorStopsClient`（`client_test.go:2844-2894`）对 `video` 和 `audio` 两种失败分别断言：`Wait2` 返回的**不是** `ErrClientEOS`，且含 `404`。注释也明确写了场景：健康的一路永远下不完、失败的一路返回 404——即使健康路仍可播放，也整体停止。
- EOS 是**集体语义**：`TestClientLifecycleStaggeredRenditionEOS`（`client_test.go:2731-2842`）中 Spanish 音轨故意拖慢，断言在它交付之前 `Wait2` 不返回，全部完成后才返回 `ErrClientEOS`。

### 3.6 错误传播的两条通道（容易混淆）

| 通道 | 触发点 | 携带内容 | 终点 |
| --- | --- | --- | --- |
| `chProcessorError` | 下载器把错误塞进 `segmentQueue`（`client_stream_downloader.go:171-173`）→ 处理器 `onProcessorError`（`client_stream_processor_fmp4.go:123-127`、`client_stream_processor_mpegts.go:75-79`） | 段下载/解码错误、EOS | 主下载器：交付前 → 立即返回；交付后 → 非 EOS 即致命 |
| `errgroup` 首错 | 处理器 `run` 直接 `return err`（如 `:74` 多轨 rendition、`:145` 找不到 leading 轨、`init.Unmarshal` 失败）；下载器 `run` 在取播放列表阶段失败（`client_stream_downloader.go:111-117`） | 结构性错误 | `errGroup.Wait()`（`client.go:317`）→ `closeError` |

由此产生一个非直观的差异：**媒体播放列表本身 404** 时不会经过 `chProcessorError`，而是直接以 `errgroup` 首错出现；同一时刻汇总循环会因 context 取消返回 `terminated`。`errgroup` 只保留**首个**非 nil 错误，而 `terminated` 只可能产生于取消之后——因此按 `errgroup` 的标准实现（错误先记录、再取消），`Wait2` 最终返回的仍是真实的 `404`，不是 `terminated`。

反过来，`Client.Close()` 引发的取消没有任何其他错误竞争，且 `runInner` 中的看守 goroutine（`client.go:288-295`）与主下载器的取消分支（`client_primary_downloader.go:310-311`、`:340-341`）都返回文本 `terminated`，所以 `Wait2` 稳定返回 `terminated`——这正是 `TestClientLifecycleCloseBlockedHTTP` 的断言（`client_test.go:2691`）。

需要注意的只是**多路同时失败**时谁获胜：只有第一个非 nil 错误被记录，两个并发失败的 stream 谁先返回由调度决定，因此错误文本在"多处同时 404"这类场景下不保证稳定。而**段 404** 走的是第一条通道，能稳定经 `chProcessorError` 传出（这正是 §3.4/§3.5 两个测试断言 `404` 的原因）。

### 3.7 剩余可用媒体能否继续呈现？

**不能。** 可以精确表述为：

- 在**元数据阶段**（`OnTracks` 之前）：整组是一个原子集合，缺一即全无。
- 在**播放阶段**（`OnTracks` 之后）：所有轨道共享同一个 `errgroup` 与同一个时间基准；任一路的非 EOS 错误都会取消整个组。健康轨道无法"脱离组"继续被调用方消费。
- 唯一"部分可用"的情况是：某路只是**慢**（不是错），此时其余轨道照常回调（`TestClientLifecycleStaggeredRenditionEOS` 中 video/English 先到），但 `Wait2` 会一直挂着直到最慢那路结束——终态仍然是全体。
- 时间基准与"放行"都只会发生一次，且 rendition 在两个阶段分别被卡住：
  - **元数据阶段**：rendition 处理器在 `setTracks` 里先发 `chTracks`、再阻塞等待 `chStartStreaming`（`client_stream_downloader.go:426-447`），而 `startStreaming` 广播要等**所有**流都交出轨道之后（`client_primary_downloader.go:324-329`）。所以任何一路缺席，其余 rendition 都会停在 `<-d.chStartStreaming`，无法进入解码。
  - **播放阶段**：`timeConvReady` 只关闭一次（`client_primary_downloader.go:151-161`），rendition 在 `waitTimeConv` 阻塞（`:163-170`）。若 leading 流在交出第一条 leading 轨的时间戳之前就失败，rendition 会停在这里，直到 `errgroup` 取消才以 `terminated` 收敛（`client_stream_processor_fmp4.go:244-253`、`client_stream_processor_mpegts.go:330-339`）。**两处都不存在 rendition 自建的兜底基准**。

---

## 4. 场景三：带宽变化播放场景的适用边界

### 4.1 已有初始选择 vs 动态切换

| 维度 | 现状 | 代码依据 |
| --- | --- | --- |
| 主播放列表刷新 | **仅下载一次**；`primaryPlaylistURL` 在首次下载后只被赋值为最终 URL（重定向基准），此后不再请求 | `client_primary_downloader.go:179-187` |
| 变体切换 | **不存在**任何切换逻辑；`leadingPlaylist` 是局部变量，生命周期止于 `run` | `:216-293` |
| 音频组切换 | 不存在；组内成员是"全订阅"而非"可切换集合" | `:251-292` |
| 时间基准 | leading 流建立、全体共享；`clientTimeConv*` 无重置入口 | `client_stream_processor_fmp4.go:229-256`、`client_time_conv_fmp4.go:22-47` |
| 带宽自适应 | 无吞吐量测量、无 ABR、无回退重试 | 全仓库无相关代码 |
| 直播位置 | `StartDistance`（默认 3）/ `MaxDistance`（默认 5） | `client.go:92-98,138-143` |

因此"带宽变化时的适用边界"是：

**适用**：码率恒定或对可用带宽有保证的播放（点播、局域网、专线）；调用方自己在外部（例如选不同的主播放列表 URL、或多次创建 `Client`）做切换。

**不适用**：

1. **移动网络 / 拥塞链路**：只会锁定在"通过白名单的变体里 BANDWIDTH 最大者"，即**最费带宽的一路**，卡顿后不会降档。
2. **首选失败无回退**：最高带宽变体的媒体播放列表 404 或 init 段异常 → 直接失败，不会尝试次高变体。
3. **中途降档不可行**：即使调用方想换，也得重建 `Client`；`OnTracks` 契约是"终身一次"，`c.tracks` 以 `*Track` 为键（`client.go:320-335`），重建前注册的 `OnData*` 回调会全部失效。
4. **落后的处理是"报错"而不是"追赶"**：直播中若某路落后超过 `MaxDistance`，`downloadNextSegment` 返回 `playback is too late`（`client_stream_downloader.go:386-388`），该错误非 EOS → 整个 client 终止。`there aren't enough segments to fill the buffer`（`:371-374`）同理。两者都**未被测试覆盖**。
5. **封装不可混用**：fMP4 变体 + MPEG-TS rendition（或反之）会在等待时间基准时因类型断言失败，报 `stream playlists are mixed MPEG-TS/fMP4`（`client_stream_processor_fmp4.go:244-253`、`client_stream_processor_mpegts.go:330-339`）；fMP4 rendition 若含多轨则报 `rendition playlists with multiple tracks are not supported`（`client_stream_processor_fmp4.go:73-75`，已由 `TestClientErrors` 覆盖）。

补充一个容易被忽略的边界：**选中的 leading 变体可能是纯音频**。若所有视频变体都因声明被过滤（例如全是 `av01.`），剩下的候选若只有音频变体，`fmp4PickLeadingTrack` 会退化为"取第一条"（`client_stream_processor_fmp4.go:15-25`），客户端将完全不交付视频，也不报错。

### 4.2 保持正常单流行为的兼容建议

目标：新增能力时，**默认路径的字节级行为不变**。按风险从低到高：

1. **默认值即旧行为**。任何变体选择/音频选择能力都应作为 `Client` 的可选字段（如 `MaxBandwidth`、`VariantSelector`），零值/null 时走现有 `pickLeadingPlaylist` 分支。不要在默认路径上修改 `>` 比较或候选顺序（并列取先者已是可观察行为）。
2. **不要改变 rendition 的"全取"语义**。`DEFAULT`/`AUTOSELECT` 单选会直接改变 `OnTracks` 的轨道数量与顺序，属于破坏性变更；若要提供，必须是显式开关，并同步更新 `TestClient` 中 `video+multiaudio` 的轨道断言（`client_test.go:1082-1119`）。
3. **`checkSupport` 的白名单扩展要单独决策**。补上 `av01.`/`vp09.`/`flac` 会让历史上"被淘汰后选中次高变体"的播放列表改为选最高变体——这是**行为变更**（虽然更像修 bug）。建议：要么保持默认白名单不变、新增 opt-in，要么作为独立修复在 CHANGELOG 中明示，并补一条多变体测试。
4. **`OnTracks` 的轨道集合与 `*Track` 指针必须终身稳定**。`OnDataAV1/OnDataH26x/...` 内部是 `c.tracks[track]` 直接索引（`client.go:225-271`），**没有存在性检查**；任何动态切换若引入新的 `*Track`，已注册回调会 panic。切换机制要么复用已有指针，要么提供明确的"轨道重建"回调并让旧指针失效可被检测。
5. **不要尝试在运行期重置时间基准**。`timeConvReady` 是只关一次的 channel，`clientTimeConvFMP4`/`MPEGTS` 的 `chLeadingNTPReceived` 同样只关一次（`client_time_conv_fmp4.go:40-47`）。跨变体切换需要新的"重设基准"协议；在协议落地前，切换应实现为"关闭旧 client、创建新 client"。
6. **保持终止语义**。`Wait2()` 的契约（`ErrClientEOS` 仅当全体结束；任一非 EOS 错误立即返回）已被多个生命周期测试锁定（`client_test.go:2731-2954`）。若未来引入"降级继续"，必须新增独立的错误/回调通道，不能复用 `Wait2` 的返回值。
7. **顺带可修但需评估的两处规范偏差**：`getRenditionsByGroup` 应同时匹配 `Type == AUDIO`（当前跨类型撞名会误取）；`AUDIO` 组 `URI==nil` 时至少可做一次"实际是否有音轨"的告警（当前静默）。

### 4.3 仍缺少的播放验收证据

以下均**无法从现有测试推出**，需要真实播放/网络环境补齐：

| 缺口 | 现状 | 需要的证据 |
| --- | --- | --- |
| 多变体选择 | 客户端测试夹具里的主播放列表**每个都只有 1 个 `EXT-X-STREAM-INF`**（`client_test.go` 中 7 处出现，分布在 `TestClient` 的 2 个 content 分支、`TestClientErrors`、`TestClientRedirect`、三个 `TestClientLifecycle*`）；`TestClient` 的 `fmp4_multiplaylist` 也只是单变体 + 多 rendition | ≥2 变体、不同 `BANDWIDTH`/`CODECS`，断言实际请求的 URL 与 `OnTracks` |
| muxer→client 闭环 | muxer 产出的 `av01`/`vp09`/`flac` 主播放列表从未被 client 消费；两者测试各自独立通过 | 把 `Muxer` 的 `index.m3u8` 直接喂给 `Client`，覆盖全部 codec（当前预期 `av01`/`vp09`/`flac` 会在 `checkSupport` 处失败） |
| 声明白名单 | `TestClientCodecs` 只覆盖**单媒体播放列表**（`client_test.go:1239-1252`） | 多变体下的 `av01`/`vp09`/`flac` 变体：确认当前会被淘汰，以及修复后的选择变化 |
| 组引用缺失 | 错误字符串无测试命中 | 1 个用例断言 `no playlist with Group ID ... found` 与"不回退到次高变体" |
| `URI` 缺失的内嵌音轨 | muxer 会生成这种清单（`muxer_stream.go:257-264`、`muxer_test.go:533-543` 中 `NAME="audio1"` 无 `URI`、German 有 `URI`），但**没有** `Client` 消费它的测试 | 混合组（1 条有 URI + 1 条无 URI）→ 断言轨道数不重复、且内嵌音轨确实存在 |
| 内嵌 + 外置重复音频 | 测试夹具主流均为纯视频（`init_video.mp4` 单轨） | 主流含 AAC 且组内也有 AAC → 断言是否出现两条音轨 |
| 降级呈现 | `TestClientLifecycleRenditionErrorStopsClient` 证明"**不**降级" | 如果这是预期语义，需要一条"明确拒绝降级"的文档化用例；若将来支持降级，需要"健康轨继续回调 + 独立通知"的验收 |
| 播放后续跑 | 所有测试在少量样本后即结束 | 长时间跑流、段过期、时钟漂移、`playback is too late` / `aren't enough segments` 分支 |
| 端到端画面/声音 | 仓库无解码或渲染断言 | 实际播放器验证（音画同步、`AbsoluteTime` 跨轨一致性） |
| 带宽变化 | 固定 HTTP handler，无吞吐量模拟 | 限速/抖动/断链下的行为记录（当前预期为"不切换、可能终止"，需实测确认） |

---

## 5. 证据矩阵

| 论断 | 源码 | 已有测试（阅读所得） |
| --- | --- | --- |
| 选最高 `BANDWIDTH` 且严格 `>` | `client_primary_downloader.go:96-104` | ❌ 无多变体用例 |
| `CODECS` 白名单全有或全无 | `client_primary_downloader.go:19-30,87` | ❌ 仅单播放列表 codec 测试 |
| 实际轨道来自 init/PMT，不读 `CODECS` | `client_stream_processor_fmp4.go:68-104`；`client_stream_processor_mpegts.go:158-180` | ✅ `TestClientCodecs`、`TestClientSkipUnsupportedTracks`（单播放列表） |
| 音频组全取，`DEFAULT`/`AUTOSELECT` 不参与 | `client_primary_downloader.go:252-292` | ✅ `TestClient`（multiaudio）、`TestClientLifecycleStaggeredRenditionEOS` |
| `URI==nil` 静默跳过 | `client_primary_downloader.go:259-261` | ⚠️ 生成端有（`muxer_test.go:533-543`），消费端无 |
| rendition 元数据仅 fMP4 非 leading 填充 | `client_stream_processor_fmp4.go:85-102`；MPEG-TS 处理器无 `rendition` 字段 | ✅ 直接：`client_test.go:1046-1080` 对比内嵌（空）与外置（English/en/Default） |
| rendition 必须单轨 | `client_stream_processor_fmp4.go:73-75` | ✅ `TestClientErrors / multiple tracks in rendition`（`client_test.go:2128-2130`） |
| 交付前任一失败 → 整体失败 | `client_primary_downloader.go:299-313` | ✅ `TestClientLifecycleErrorBeforeTracks` |
| 交付后任一非 EOS → 整体终止 | `client_primary_downloader.go:331-339` | ✅ `TestClientLifecycleRenditionErrorStopsClient` |
| EOS 需全体 | `client_primary_downloader.go:333-345` | ✅ `TestClientLifecycleStaggeredRenditionEOS` |
| 混合封装被拒 | `client_stream_processor_fmp4.go:244-253` | ❌ 无 |
| 组缺失硬错误 | `client_primary_downloader.go:252-255` | ❌ 无 |
| 主播放列表只取一次 | `client_primary_downloader.go:179-187` | ❌ 无（无刷新断言） |
| 无动态切换 | 全仓库无 ABR 代码 | ❌ 无 |
| `playback is too late` / 缓冲不足 | `client_stream_downloader.go:371-374,386-388` | ❌ 无 |
| 混用 TS/fMP4 由时间基准的类型断言拒绝 | `client_stream_processor_fmp4.go:244-253`、`client_stream_processor_mpegts.go:329-339` | ❌ 无 |

---

## 6. 静态阅读的边界与待验证项

1. **未运行任何测试**：本机 `GOROOT` 不可用，离线无法拉取 `go1.26.0`。上述"已有测试支持"是对测试源码的覆盖判断，不代表 `go test` 通过。
2. **`mediacommon` 的行为未验证**：
   - `fmp4.Init.Unmarshal` 是否会对未知 codec 直接报错，决定了 `fromFMP4` 返回 `nil` 的分支是否可达（§2.2 第三点）；
   - `fmp4PickLeadingTrack` 在 `init.Tracks` 为空时会索引 `[0]`（`client_stream_processor_fmp4.go:24`），是否可能到达取决于 mediacommon 是否允许 0 轨 init。
3. **网络行为未验证**：重定向后的相对 URL 基准（`client_primary_downloader.go:187,223,264`）在跨域/跨路径场景下的实际表现（`TestClientRedirect` 覆盖了单流与单变体重定向，未覆盖 rendition 重定向）。
4. **HLS 规范符合性未逐条比对**：本报告只描述实现行为，不评估 RFC 8216 的合规程度；`DEFAULT`/`AUTOSELECT` 的选择语义、按 `TYPE` 限定 `GROUP-ID` 等属于实现偏差，已在 §3.2、§4.2 标注。

---

## 附：关键代码位置索引

| 主题 | 位置 |
| --- | --- |
| 主播放列表下载（唯一一次） | `client_primary_downloader.go:179-187` |
| 变体白名单 | `client_primary_downloader.go:19-30` |
| 变体选择 | `client_primary_downloader.go:84-105,216-220` |
| 音频组解析 | `client_primary_downloader.go:107-120,251-292` |
| `URI==nil` 跳过 | `client_primary_downloader.go:257-261` |
| 轨道汇总 / `OnTracks` | `client_primary_downloader.go:299-322`、`client.go:320-335` |
| 终止与 EOS 聚合 | `client_primary_downloader.go:331-345` |
| fMP4 实际轨道推导 | `client_stream_processor_fmp4.go:67-134` |
| MPEG-TS 实际轨道推导 | `client_stream_processor_mpegts.go:145-190` |
| 采样交付 / 负 PTS 丢弃 | `client_track.go:23-53`、`client_track_processor_fmp4.go:111-138` |
| 直播起点与"太晚" | `client_stream_downloader.go:354-400` |
| 时间基准（只初始化一次） | `client_primary_downloader.go:151-170`、`client_time_conv_fmp4.go:22-47` |
| muxer 侧 `URI` 生成约定 | `muxer_stream.go:241-267` |
