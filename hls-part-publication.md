# LL-HLS 部分片段提前可见性与可播放内容的责任边界分析

分析对象：本仓库（gohlslib v2）实际源码与测试，重点是低延迟变体（`MuxerVariantLowLatency`）下
部分片段（part）在尚未完整生成时即通过 `EXT-X-PRELOAD-HINT` 与阻塞式播放列表刷新对外可见
的机制。所有结论以仓库材料为依据，未运行服务。

## 1. 提前可见性的三层结构

muxer 对外暴露的"部分片段"存在三种不同的可见性等级，责任边界各不相同：

1. **已发布 part（可播放内容）**：`rotateParts()` 中 part 被 `finalize()` 后，才追加到
   `segment.parts` 并注册内容 handler（`muxer_stream.go:699-723`）。只有此时 part 才进入
   播放列表的 `EXT-X-PART`（`muxer_stream.go:531-543, 557-569`）。内容 handler 读取的是
   已 finalize 的存储（`pkg/storage/part_ram.go:24-26` 直接读 buffer 快照），因此
   **凡是出现在 `EXT-X-PART` 中的 URI，取到的必然是完整 part**。

2. **预加载提示（提前可见、内容尚不存在）**：播放列表末尾总是输出
   `EXT-X-PRELOAD-HINT`，URI 指向 `s.nextPartID`——即正在写入、尚未 finalize 的 part
   （`muxer_stream.go:571-579`）。代码注释明确说明该提示必须始终存在，
   "otherwise hls.js goes into a loop"（`muxer_stream.go:571-572`）。
   提示 URI 的 handler 是一个**阻塞占位符**：在上一次 `rotateParts()` 时注册
   （`muxer_stream.go:725-754`），请求进入后在条件变量上等待
   `s.nextPartID > capturePartID`，然后重新查表转发给（此时已注册的）内容 handler。

3. **阻塞式播放列表刷新（对未来内容的订阅）**：携带 `_HLS_msn`/`_HLS_part` 的媒体播放
   列表请求在 `hasContent() && hasPart(msn, part)` 满足前阻塞在 `s.cond.Wait()`
   （`muxer_stream.go:332-359`）。这是对"尚未生成的后续媒体"的等待订阅。

关键时序保证：占位 handler 与内容 handler 注册在同一把互斥锁（`m.mutex`，流间共享，
`muxer.go:600-612`）保护的关键区内完成，且 `nextPartID++` 先于内容 handler 注册
（`muxer_stream.go:689-723`）。被唤醒的占位请求在持锁状态下调用 `getPathHandler`
（`muxer_stream.go:747-749`），因此**不存在"提示已承诺但内容 handler 尚未注册"的窗口**；
占位 handler 被内容 handler 在同一路径上覆盖注册，先到请求阻塞、后到请求直读。

## 2. 尚未发布的部分片段：何时属于正常等待

### 2.1 播放列表刷新中的等待

`handleMediaPlaylist` 对 LL 变体的判定顺序（`muxer_stream.go:332-359`）：

- **立即 400（非正常等待）**：
  - `msn > nextSegmentID + 1`（超出"当前开放分段+1"，即超过规范允许的前瞻范围）；
  - `msn < nextSegmentID - (len(segments) - 1)`（请求的分段已被窗口淘汰）。
  注释直接引用规范："the server SHOULD immediately return Bad Request"
  （`muxer_stream.go:344-348`）。`TestMuxerExpiredSegment`（`muxer_test.go:2602-2652`）
  验证了过期 msn 返回 400。
- **正常等待**：msn 落在 `[nextSegmentID-(len(segments)-1), nextSegmentID+1]` 窗口内，
  但 `hasPart(msn, part)` 尚未为真——即该 part 属于尚未生成的后续媒体。请求阻塞在
  `cond.Wait()`，由每次 `rotateParts`/`rotateSegments` 后的 `cond.Broadcast()`
  （`muxer.go:609, 645`）唤醒重判。这是规范意义上 blocking reload 的正常等待。
- 注意 `hasContent()` 的前置条件：fMP4 变体要求 `len(s.segments) >= 2`
  （`muxer_stream.go:272-277`），即首个分段 finalize 之前一切播放列表请求都等待，
  这也属于正常等待（流尚未有可播放内容）。

### 2.2 预加载提示 GET 中的等待

对提示 URI 的 GET 在 part finalize 前阻塞（`muxer_stream.go:733-745`），这同样属于正常
等待：提示指向的永远是"正在生成的那一个 part"。`TestMuxerPreloadHint`
（`muxer_test.go:2705-2818`）验证：对 `part1.mp4` 提示 URI 的请求在 500ms 内不应返回，
写入下一个 IDR 触发 part 轮换后，请求返回且载荷可解析为 `SequenceNumber=1` 的 fMP4 part。

### 2.3 窗口边界的实际形态

LL 变体起始 `nextSegmentID=7`（`muxer.go:378-381`），首个分段 finalize 时补 7 个
`muxerGap`（`muxer_stream.go:813-819`），随后窗口淘汰保持 `len(segments) == SegmentCount`
（`muxer_stream.go:852-868`）。因此 400 判定的下界随每次分段轮换单调右移，
"正常等待"的 msn 集合是一个随直播边缘滑动的窗口。

## 3. 越过完整分段末尾的部分编号如何衔接

`hasPart`（`muxer_stream.go:279-302`）实现了规范条款（注释原文引用）：

> If the Client requests a Part Index greater than that of the final Partial Segment
> of the Parent Segment, the Server MUST treat the request as one for Part Index 0
> of the following Parent Segment.

具体行为分两支：

- **已发布分段**（在 `s.segments` 中命中）：若 `partID >= len(seg.parts)`，则
  `segmentID++; partID = 0` 并继续循环——跨分段衔接可以连续推进多段，直到命中某个
  已发布分段的有效 part 或抵达开放分段。
- **当前开放分段**（`segmentID == s.nextSegmentID`）：只检查
  `partID < len(nextSegment.parts)`，**不做越界映射**。因为开放分段的 part 列表仍在
  增长，越界编号此时只意味着"尚未生成"，应继续等待而非映射到下一分段。

衔接的连续性由编号体系保证：part ID 是**跨分段全局递增**的（`nextPartID` 在
`rotateParts` 与 `rotateSegments` 路径中都只增不减，`muxer_stream.go:689, 911-921`），
且 fMP4 part 的 `SequenceNumber` 直接取全局 part ID（`muxer_part.go:42`），
`TestMuxerFMP4SequenceNumber`（`muxer_test.go:2464-2556`）验证 part0..2 的
SequenceNumber 与编号一致。播放列表中分段内 part 索引（`_HLS_part` 的语义）与文件名为
全局编号的映射由服务端内部完成，客户端无需感知分段边界。

## 4. 窗口淘汰后旧请求如何结束

窗口淘汰发生在 `rotateSegments`（`muxer_stream.go:851-868`）：最老分段的所有 part 路径
与分段路径被 `unregisterPath`，存储被 `close()`（RAM 为 no-op，磁盘删除文件），
`segmentDeleteCount++` 体现为播放列表的 `EXT-X-MEDIA-SEQUENCE` 前移。三类旧请求的终结方式：

1. **阻塞中的播放列表请求（msn 已过期）**：被 Broadcast 唤醒后命中下界检查，返回
   **400**（见 §2.1，有测试覆盖）。
2. **对已淘汰 part/分段 URI 的新 GET**：`muxerServer.handle` 查不到 handler 时**什么都不
   写**（`muxer_server.go:36-46`）——在真实 net/http 下表现为 200 空响应而非 404。这是
   一个有意的静默行为还是疏漏，仓库材料未给出说明（见 §7 证据缺口）。
3. **阻塞中的预加载提示请求**：part ID 单调递增，被淘汰的永远是最旧 part，而提示请求
   等待的永远是最新 part，二者不会相交；占位 handler 唤醒后持锁查表，淘汰无法插入
   其间。因此阻塞中的提示请求只会以"内容就绪"或"流关闭（404）"两种方式结束。

## 5. 从提示转为读取媒体：连续播放依赖的条件

结合独立解码标记与增量播放列表，客户端（以本仓库 client 为参照，
`client_stream_downloader.go:179-202`）获得连续播放依赖以下条件：

1. **提示的持久性**：客户端循环是"下载提示 URI → 推入分段队列 → 重新拉播放列表"，若某次
   播放列表中 `PreloadHint` 消失，客户端直接报错 `"preload hint disappeared"`
   （`client_stream_downloader.go:198-200`）。muxer 侧"提示必须始终存在"的注释与此互为
   契约。
2. **增量更新保留 EXT-X-MAP**：`_HLS_skip=YES` 触发 delta update
   （`muxer_stream.go:321-324, 493-508`），但 `EXT-X-MAP` 必须保留在增量播放列表中，
   注释说明 "otherwise iOS clients stop playback"（`muxer_stream.go:483`）。
   `TestMuxerDeltaUpdate`（`muxer_test.go:2654-2703`）验证多次增量响应中 MAP URI 稳定
   且 SKIP 随窗口长度变化。skip 边界为 `targetDuration * 6`，满足规范
   "Skip Boundary MUST be at least six times the Target Duration"
   （`media_server_control.go:27-29` 的注释）。
3. **INDEPENDENT 标记的真实性**：part 含引导轨同步样本时 `isIndependent=true`
   （`muxer_part.go:78-80`），播放列表据此输出 `INDEPENDENT=YES`
   （`muxer_stream.go:538-542, 564-568`）。客户端可仅从标记为 INDEPENDENT 的 part 起
   解码；muxer 保证标记与实际样本内容一致（测试 `muxer_test.go:703-711, 2536-2539`
   验证含 IDR 的 part 有标记、后续 part 无标记）。
4. **PART-TARGET 的稳定性**：规范要求 part 时长 ≥ PART-TARGET 的 85%，因此
   `partTargetDuration` 一经设定不允许变化，变化时通过 `OnEncodeError` 上报
   "this will cause an error in iOS clients"（`muxer_stream.go:772-785`）；分段器侧在
   首次分段轮换后冻结调整值（`muxer_segmenter.go:573`），并用
   `findCompatiblePartDuration` 选取与所有样本时长兼容的 part 时长
   （`muxer_segmenter.go:488-507`）。`PART-HOLD-BACK = partTarget * 2.5`
   （`muxer_stream.go:468`），满足规范"至少两倍 Part Target"。
5. **init 文件先行**：fMP4 播放列表始终携带 `EXT-X-MAP`，init 在首个分段 finalize 后
   生成并缓存（`muxer_stream.go:870-876, 585-630`）；客户端先下载 init 再进入 LL 循环
   （`client_stream_downloader.go:122-145`）。
6. **时间连续性**：客户端用最后一个分段的 `PROGRAM-DATE-TIME` 加各 part 时长推算提示
   part 的时间（`client_stream_downloader.go:54-71`），因此播放列表中最近两个分段必须
   携带 `DateTime`（`muxer_stream.go:527-529`）。

## 6. 媒体生成失败或流关闭时的状态区分

### 6.1 已公开内容

- 已 finalize 的 part/分段：handler 已注册，读取的是 finalize 后的存储快照，**不受后续
  写入失败影响**。
- `Close()` 时：播放列表类请求一律 404（`muxer.go:706-710`、`muxer_stream.go:339-342`，
  `TestMuxerCloseBeforeData` 覆盖）；已发布分段/part 的内容 handler **不注销**，但
  `stream.close()` 会对每个分段调用 `storage.Remove()`（`muxer_stream.go:160-175`）。
  RAM 存储的 `Remove` 是 no-op（`pkg/storage/file_ram.go:28-30`），关闭后已发布内容
  仍可读取；磁盘存储则物理删除（`TestMuxerSaveToDisk` 验证关闭后文件消失，
  `muxer_test.go:2019-2031`）。两种后端的关闭后行为不对称，调用方不应依赖其一。

### 6.2 等待中的请求

- **流关闭**：`Close()` 置 `closed=true` 并 `cond.Broadcast()`（`muxer.go:480-492`），
  所有阻塞中的播放列表请求与提示请求被唤醒后返回 **404**
  （`muxer_stream.go:339-342, 734-738`；`TestMuxerCloseWhileRequestStream` 覆盖播放
  列表路径）。关闭时正在写入的 part/分段以 `finalize(0)` 收尾但**不注册、不发布**
  （`muxer_stream.go:167-174`），即"已公开"与"在制"严格分离：在制内容永不可见。
- **媒体生成失败**：写入错误（如 `reached maximum segment size`，`muxer_part.go:69-71`）
  沿 `Write*` 调用链返回给调用方，**不触发 cond 广播、不置错误态**。阻塞中的 HTTP
  请求继续等待，唯一的终结途径是调用方随后调用 `Close()`。`OnEncodeError` 仅用于
  非致命问题（target/part 时长漂移），不参与请求生命周期。

## 7. 结论：已有保证、调用方前提与证据缺口

### 7.1 代码已有保证（有源码与测试支撑）

- `EXT-X-PART` 中的 URI 必然对应已 finalize 的完整 part；提示 URI 与内容 URI 的注册
  时序在互斥锁下无竞争窗口。
- 阻塞刷新对越窗 msn 立即 400（`TestMuxerExpiredSegment`）；对窗内未来媒体正常等待；
  关闭时一律 404（`TestMuxerCloseWhileRequestStream`、`TestMuxerCloseBeforeData`）。
- 越界 part 索引按规范映射到下一分段的 part 0（仅对已发布分段）。
- 提示 GET 在 part 就绪前阻塞、就绪后返回完整 fMP4 part（`TestMuxerPreloadHint`）。
- 增量更新保留 MAP、SKIP 计数随窗口变化（`TestMuxerDeltaUpdate`）；part 全局编号与
  fMP4 SequenceNumber 一致（`TestMuxerFMP4SequenceNumber`）。

### 7.2 调用方前提（代码不强制，违反则无保证）

- 写入失败后调用方必须自行决定重试或 `Close()`；否则阻塞中的客户端请求无限等待。
- 输入时间戳需单调且样本时长稳定，否则触发 part/target 时长漂移的 `OnEncodeError`，
  iOS 客户端将出错（代码仅上报，不阻止）。
- LL 变体要求 `SegmentCount >= 7`（`muxer.go:313-316`），窗口语义以此为基础。
- 关闭后的可读性依赖存储后端（RAM 可读、磁盘删除），调用方不应把 muxer 当作关闭后的
  归档服务。

### 7.3 证据缺口（仓库材料无法判定）

- 已淘汰 part/分段 URI 的 GET 得到的是"无 handler 的空响应"（隐式 200 空体）而非显式
  404/410；无测试覆盖，无法从仓库材料判定这是契约还是疏漏。验收时建议明确期望状态码。
- 占位提示 handler 中 `getPathHandler` 返回 nil 的分支（`muxer_stream.go:751-753`）
  在正常时序下不可达，但亦无防御性输出；无测试覆盖该分支。
- 客户端 LL 循环不使用 `_HLS_msn`/`_HLS_part` 阻塞刷新，而是"提示 GET 阻塞 + 立即重拉
  播放列表"（`client_stream_downloader.go:179-202`）；服务端阻塞刷新路径仅有 muxer 侧
  测试，端到端（本库客户端对本库服务端）的阻塞刷新行为无集成测试验证。

### 7.4 验收判据建议

1. 任一时刻抓取的媒体播放列表中，所有 `EXT-X-PART` URI 并发 GET 均返回 200 且可解析为
   完整 fMP4 part；`EXT-X-PRELOAD-HINT` URI 的 GET 在下一次 part 轮换前不返回，轮换后
   返回 200 且 SequenceNumber 等于提示时的 `nextPartID`。
2. 以 `_HLS_msn=<窗口最老-1>` 请求播放列表立即得到 400；以窗口内未来 msn/part 请求则
   阻塞，且在对应 part 出现在播放列表后返回 200。
3. 请求某已发布分段的 `_HLS_part=<该分段 part 数>`，等价于请求下一分段的 part 0。
4. 写入中途 `Close()`：所有在途播放列表/提示请求以 404 结束；已发布 part 在 RAM 后端
   下仍可读取；在制 part 的任何 URI 均不可见。
5. 持续以 `_HLS_skip=YES` 拉取增量播放列表：响应始终含 `EXT-X-MAP` 且 URI 稳定，
   `SKIPPED-SEGMENTS` 与窗口长度一致，`EXT-X-PRELOAD-HINT` 始终存在。
6. 播放列表中 `INDEPENDENT=YES` 的 part 必含引导轨随机访问点；从任一此类 part 起可
   独立解码（结合 init 文件）。
