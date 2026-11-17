# HLS 复用器初始化参数生命周期分析（gohlslib v2 静态分析）

分析对象：本仓库 `task/60` 分支（基线提交 `b771acf`）。
分析方法：仅静态阅读源码与测试，未启动推流或播放进程，未做任何解码验证。
涉及文件：`muxer.go`、`muxer_stream.go`、`muxer_segmenter.go`、`muxer_track.go`、`muxer_segment_mpegts.go`、`from_to_fmp4.go`、`from_to_mpegts.go`、`client_stream_downloader.go`、`client_stream_processor_fmp4.go`、`muxer_test.go`。

---

## 1. 三类信息的确定时点

### 1.1 输入参数（Track.Codec 中的 SPS/PPS/VPS）

- 调用方在 `Muxer.Start()` 之前通过 `Track{Codec: &codecs.H264{SPS, PPS}, ClockRate: 90000}` 提供初始参数（`track.go:8-26`）。`Start()` 只校验时钟频率、轨道数量与变体约束（`muxer.go:243-322`），**不校验** SPS/PPS 是否与后续码流一致。
- 启动后，参数有一个"收集窗口"：`muxerSegmenter.writeH264` / `writeH265` 每收到一个访问单元（AU），都会扫描其中的 SPS/PPS（H264）或 VPS/SPS/PPS（H265）NALU，并用码流内的版本**覆盖** `codec.SPS/PPS/VPS`——但前提是 `!track.stream.initFilePresent`（`muxer_segmenter.go:283-290`、`218-230`；AV1/VP9 同理，见 `:119`、`:161-180`）。
  - 这意味着调用方启动时填错或填空参数也能被码流内参数纠正，`TestMuxerInStreamParams`（`muxer_test.go:2158-2323`）专门验证了这一点：初始填入"wrong initial params"，最终 init 中仍是码流内的 VPS/SPS/PPS。
- 首个可独立解码画面之前的样本被**静默丢弃**：`firstRandomAccessReceived` 为 false 时，非随机访问 AU 直接 `return nil`（`muxer_segmenter.go:294-303`、H265 在 `:234-243`）；首个随机访问 AU 到达时才创建 DTS 提取器（`:300-303`）。因此参数收集的实际窗口是：从 `Start()` 到**第一个分段轮换（rotateSegments）**，而不是到首个 IDR——首个 IDR 之后、首个分段结束之前收到的参数仍会被收集。

### 1.2 媒体分段（segment / part）

- 分段在首个随机访问样本到达后由 `createFirstSegment` 创建（`muxer_stream.go:632-683`），此后按"随机访问 + 超过 SegmentMinDuration"轮换（`muxer_segmenter.go:563-574`）。
- fMP4 样本负载是**整个 AU 的 AVCC 序列化**（`muxer_segmenter.go:338-341`、`:250-253`），即码流内携带的 SPS/PPS NALU 会被原样打进分段负载，不会被剥离。
- MPEG-TS 变体没有 init 概念：`toMPEGTS` 对 H264 只返回空的 `&tscodecs.H264{}`（`from_to_mpegts.go:30-31`），参数完全依赖码流内带内传输（`muxer_segment_mpegts.go:96-101` 原样写 AU）。

### 1.3 播放列表引用的初始化信息（EXT-X-MAP / init.mp4）

- init 文件**只生成一次**：在第一个分段被 finalize 的 `rotateSegments` 中，`if s.variant != MuxerVariantMPEGTS && !s.initFilePresent` 调用 `generateAndCacheInitFile()`，随后置 `initFilePresent = true`（`muxer_stream.go:870-876`）。
- `generateAndCacheInitFile` 用**当时**的 `track.Codec` 经 `toFMP4` 编出 `fmp4.Init`，把字节缓存进一个固定路径 `<prefix>_<streamID>_init.mp4` 的 HTTP 处理器（`muxer_stream.go:585-630`、`initFilePath` 在 `:34-36`）。此后该路径永远返回这份冻结字节，不再重新生成。
- 媒体播放列表始终只引用这一个 init URI：`generateMediaPlaylistFMP4` 无条件输出单个 `EXT-X-MAP`（`muxer_stream.go:483-491`），注释明确说明"EXT-X-MAP must be kept in delta updates too, otherwise iOS clients stop playback"（`:483`）。播放列表结构（`playlist.Media.Map`）本身也只有单一 Map 字段，无法表达"某分段起切换 init"。
- 时序保证：fMP4 变体下 `hasContent()` 要求 `len(s.segments) >= 2`（`muxer_stream.go:272-277`），即任何播放列表被成功下发之前，第一个分段必然已 finalize、init 必然已生成。因此**客户端不可能拿到"参数尚未冻结"状态的播放列表**。
- 多码率主播放列表（index.m3u8）的 `RESOLUTION`/`FRAME-RATE`/`CODECS` 在**每次请求时**从 `track.Codec.SPS` 现算（`muxer_stream.go:206-233`、`codecparams.Marshal`），但由于 `initFilePresent` 之后 codec 字段不再被更新，这些值实际上也冻结在 init 生成时刻。`TestMuxerDynamicParams` 验证了：SPS 切换为 720p 后，index.m3u8 仍报告 `RESOLUTION=1920x1080`（`muxer_test.go:2121-2129`）。
- 例外：MPEG-TS 变体从不设置 `initFilePresent`（`muxer_stream.go:870` 的守卫），所以 `codec.SPS/PPS` 会被每个 AU 持续更新——但这只影响 index.m3u8 里实时变化的分辨率展示，不影响分段内容（分段本来就带内携带参数）。

## 2. 参数在 init 公开后变化：现有行为

以 `TestMuxerDynamicParams`（`muxer_test.go:2036-2156`）为权威证据：

1. 用 1080p SPS 写入若干 AU → 第一个分段轮换时生成 init（含 1080p SPS）。
2. 之后写入携带 720p SPS 的 AU，`WriteH264` **返回 nil**，不报错。
3. 再次请求 init：`require.Equal(t, init1, init2)`（`muxer_test.go:2155`）——init 字节完全不变。
4. index.m3u8 仍显示旧分辨率（`:2128`）。

即：**输入被接受 ≠ 热更新生效**。`initFilePresent` 守卫使新参数被静默丢弃（既不进入 init，也不更新主播放列表），但新参数 NALU 仍随 AU 进入后续分段的样本负载。

### 错误可见性

- **不可见**。`WriteH264`/`WriteH265` 对参数变化不返回错误；`OnEncodeError` 目前只覆盖两类情况：分段时长变化（`muxer_stream.go:929-933`）和 part 时长变化（`muxer_stream.go:781-784`），均与编码参数无关。
- 调用方辨别支持边界的唯一途径是隐式约定：**"fMP4 变体下，编码参数只允许在第一个分段 finalize 之前变化；之后的变化会被静默忽略"**。这个边界没有公开 API 可查询（`initFilePresent` 是私有字段），调用方只能自行推断"已经写满了一个 SegmentMinDuration 且出现过一次分段轮换"。
- 潜在的不一致是**静默**的：旧 init（旧 SPS/PPS）+ 新分段（码流内新 SPS/PPS）。是否可解码取决于播放器实现：
  - 依赖 init 中 avcC/hvcC 带外参数的解码器会用旧参数初始化，遇到引用新参数集的画面可能花屏、丢帧或解码失败；
  - 会解析样本负载中带内参数集的解码器（gohlslib 不剥离 AU 中的 SPS/PPS，见 1.2）可能自愈。
  - **以上两种结果均未在本仓库得到验证**（见第 4 节）。

### 客户端侧的对应行为

gohlslib 自带 Client 只在启动时按第一个播放列表的 `EXT-X-MAP` 下载**一次** init（`client_stream_downloader.go:122-145`），之后即使播放列表中 Map URI 变化也不会重新下载；轨道集合从这份 init 派生后固定（`client_stream_processor_fmp4.go:67-114`）。因此即使服务端未来支持 init 热更新，当前 Client 也不会感知——兼容方案必须同时改两端。

## 3. 连续播放期间参数切换的兼容方案（建议）

目标：旧分段仍可用旧 init 解释，新分段不误用旧配置。

### 方案 A（推荐）：分段代际（generation）+ 逐段 EXT-X-MAP

1. 检测到码流内 SPS/PPS/VPS 与已冻结值不同（去掉 `!initFilePresent` 守卫，改为比较后触发）时：
   - 要求变化点必须是随机访问 AU（H264/H265 本就只在 `randomAccess` 时切分段，天然满足）；
   - 递增代际计数，生成新 init 字节，注册到新 URI（如 `<prefix>_<streamID>_init<gen>.mp4`），**旧 URI 保留继续服务**，保证已下发播放列表中的旧分段仍可解码；
   - 在媒体播放列表中，从第一个使用新参数的分段起插入新的 `EXT-X-MAP`（HLS 规范允许 EXT-X-MAP 出现在分段之间，作用于其后的所有分段直到下一个 EXT-X-MAP），并建议同时打 `EXT-X-DISCONTINUITY`。
2. 成立条件：
   - 播放列表层需要扩展：`playlist.Media` 目前只有单一 `Map` 字段（`muxer_stream.go:489-491`），需支持挂在分段上的 Map；delta update 逻辑（`muxer_stream.go:493-508`）也要保证跳过的分段不丢失其对应的 MAP 上下文；
   - 参数变化必须发生在分段边界（随机访问点），否则一个分段内会混用两套参数，单一 EXT-X-MAP 无法表达；
   - LL-HLS 的 part 粒度同理：part 属于某个分段，MAP 切换应与分段轮换对齐；
   - Client 端需改为监视 Map URI 变化并重新下载 init、重建轨道（当前 `client_stream_downloader.go:122` 只下载一次）；
   - 磁盘模式（`Directory != ""`）下 `savePlaylists`（`muxer.go:680-699`）与 init 落盘（`muxer_stream.go:606-611`）要同步处理多代际文件。
3. 调用方可见性：新增参数变化时通过 `OnEncodeError`（或新回调）通知，使"被接受"与"已生效"可区分。

### 方案 B（现状即可用的保守边界）：参数变化 = 重启 Muxer

- `Close()` 旧 Muxer、用新参数新建 Muxer。每次 `Start()` 生成随机 `prefix`（`muxer.go:95-103`，注释明确说明这是为了防止缓存串用），新 prefix 使 init/分段 URI 全部换新，天然避免新旧配置混淆。
- 成立条件：调用方自行保证旧 Muxer 的播放列表生命周期结束（客户端会拿到 404/新流）；不保证连续播放，画面会中断。这是当前代码**唯一确定正确**的参数切换路径。

### 方案 C（部分场景缓解，不推荐单独依赖）：依赖带内参数

- 由于分段负载保留了码流内 SPS/PPS（1.2 节），某些播放器可自愈。但 init 中的 avcC/hvcC 与样本不一致属于规范外的脆弱状态，且未经验证（第 4 节），不能作为正式方案。

## 4. 尚未得到验证的解码结果（静态分析声明）

以下结论**没有**任何测试或运行证据支撑，仅为代码阅读推断：

1. **新参数分段的可解码性**：`TestMuxerDynamicParams` 只断言 init 字节不变和播放列表文本（`muxer_test.go:2149-2155`），从未解码 seg2/seg3 验证 720p 画面能否用 1080p 的 init 解出。仓库内没有任何测试对产出的分段做真实解码。
2. **带内参数自愈**：分段负载中保留的新 SPS/PPS NALU 是否足以让 hls.js、iOS AVPlayer、ffmpeg 等主流解码端绕过过期的 avcC/hvcC，未验证；不同实现行为可能不同。
3. **DTS 提取的连续性**：`h264.DTSExtractor`/`h265.DTSExtractor` 在首个随机访问点创建后跨参数切换持续工作（`muxer_segmenter.go:300-305`），SPS 变化（尤其改变 POC 相关字段、max_num_reorder_frames 等）后 DTS 是否仍单调正确，未验证。
4. **主播放列表分辨率冻结的下游影响**：index.m3u8 长期报告旧分辨率（`muxer_test.go:2128` 证实）对播放器选路/渲染的实际影响，未验证。
5. **MPEG-TS 变体**：虽然无 init 问题，但参数切换后 `mpegts.Writer` 输出是否被各播放器接受（PAT/PMT 不变、ES 内参数集变化），未验证。
6. **并发边界**：参数收集（写路径，无锁）与 init 生成（`rotateSegments` 内持 `m.mutex`，`muxer.go:637`）依赖调用方串行调用 `Write*`，多 goroutine 并发写同一轨道时参数收集与 init 快照的竞态未验证。

## 5. 结论

- 输入参数的收集窗口：`Start()` → 第一个分段 finalize（fMP4）；MPEG-TS 变体则持续收集但仅影响主播放列表展示。
- init 信息在第一个分段轮换时一次性冻结并经固定 URI 公开；媒体分段在此之后持续产出；播放列表永远引用那一份冻结 init。
- init 公开后的参数变化：**输入被接受（Write 返回 nil），但热更新未发生**，错误完全不可见，调用方无 API 可辨别，只能遵守"首分段完成后参数不可变"的隐式约定。
- 正确的连续播放参数切换需要方案 A 所述的代际化 init + 逐段 EXT-X-MAP + 客户端配合；在其实现前，方案 B（重启 Muxer 换 prefix）是唯一确定安全的路径。
