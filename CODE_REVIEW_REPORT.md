# diago 深度代码审查报告

> 首轮审查对象：`github.com/sjlit/diago` @ commit `4c5b8a4`（main 分支）
> 首轮审查方式：三路并行深度审查（架构与 API 设计 / 并发与资源管理 / 协议正确性与工程质量），高严重度条目逐一读源码核实
> **第二轮复审：2026-09-07 @ commit `3fcb7f7`**（基线以来 36 个提交），逐项核实下表状态并新增第七节
> 图例：✅ 已修复 ｜ ⚠️ 部分修复 ｜ ❌ 待修复（附建议）

---

## 总体评价

媒体底层（jitter buffer、sequencer、DTLS/SDES 密钥协商、SignalOption 层）质量不错，`Diago → DialogSession → DialogMedia` 分层清晰。但库处于**迁移中途状态**：存在可被网络报文直接打挂进程的 panic、必然发生的锁误用与资源泄漏、硬编码导致的真实互通性缺陷，以及三套并存且互相冲突的 API 风格。

> **第二轮复审更新**：首轮列出的 P0 全部修复，P1/互通性大项（动态 PT、RTCP 统计、多 m= SDP、digest 加固、ticker 泄漏、Finalize 出锁、a=inactive 等）已在 `4c5b8a4..3fcb7f7` 的 36 个提交中陆续关闭，详见各条目提交号。剩余未修项收敛为：DTLS 服务端角色配置缺陷（新发现，第七节 N1）、Bridge 生命周期、opus 能力门控、Hold 方向校验、qop、API 收敛类。

---

## 一、可被远程触发或按文档使用必然出错的缺陷（P0）

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 1 | ✅ | `media/sdp/sdp.go:255` | SDP 含裸 `\n` 空行时 `line[lenline-2]` 索引 `-1` panic——一个畸形 SIP 报文即可 crash 服务器（sipgo 分发链无 recover）。**另修复**：EOF 时最后一行（无结尾 CRLF 的合法 SDP）被静默丢弃 |
| 2 | ✅ | `media/rtp_packet_reader.go:159` | RTP 读热路径 `panic("payload calc do not match")` → 改为返回错误 |
| 3 | ✅ | `bridge.go:656` | BridgeMix poll 流 goroutine 对不变量违反 `panic` → 记日志继续 |
| 4 | ✅ | `bridge.go:754` | `unmixStream` 长度守卫被注释，短缓冲带陈旧字节上线路 → 按 `min(len)` 且 16-bit 对齐截断 |
| 5 | ✅ | `audio/utils.go` | `PCMMix`/`PCMUnmix` 奇数长度输入越界 panic（混音 goroutine 无 recover）→ 三方缓冲最小值向下取偶 |
| 6 | ✅ | `audio/monitor_pcm.go` `interleave` | 交错写 WAV 时奇数尾部块 panic 风险 → 尾块取偶 |
| 7 | ✅ | `dialog_session.go:72`、`diago.go:373` | NOTIFY/INFO 缺 Content-Type 时 `req.ContentType().Value()` nil 解引用 → nil 检查 + 400 |
| 8 | ✅ | `bridge.go:158-178` | `ProxyMediaControl` 同时后台+同步启动两次 `proxyMedia`：RTP 被双份转发、随机拆包，与文档语义相反 → 重构为单次后台启动 + stop 中断/恢复 |
| 9 | ✅ | `media/rtp_session.go:454` | `Monitor()` 无缓冲 `errchan`：reader goroutine 永久阻塞在发送上，后续 `MonitorClose()` 的 `monitorWG.Wait()` 死锁 → 缓冲 1 |
| 10 | ✅ | `dialog_server_session.go:228-239` | 早期媒体（183）后 `Answer()` 跳过 `sess.Finalize()`，DTLS 握手永不执行、无声媒体 → 补齐（`MonitorBackground` 已由 ProgressMedia 启动，不重复） |
| 11 | ✅ | `dialog_session.go:439-446` | `ReferTransaction` 构造从不设置 `d` 字段，`Accept()`/`sendNotify()` 一调即 nil panic → 该 API 整体删除（见 #47） |
| 12 | ✅ | `dialog_client_session.go:781/795/565`、`dialog_server_session.go:717/731/465`、`dialog_client_session.go:768` | 未应答通话调用 `Hold`/`Unhold`/`ReInvite` 直接 nil panic → 统一返回 `"dialog session not answered"`（对齐 bridge.go 既有文案）；`handleReInviteACK` 补 nil 守卫 |
| 13 | ✅ | `diago.go:842-873` | 注册循环对 403/404 等不可重试 4xx 无限重试（无退避上限）→ 4xx（保留 408/491 瞬态码）立即返回错误 |

## 二、并发与资源管理

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 14 | ✅ | `media/rtp_packet_writer.go:153-168` | `Write`/`WriteSamples` 持 **RLock** 修改 `nextTimestamp`/`seqWriter`/`packet`（锁形同虚设）；`lastSampleTime` 锁外裸写 → 改写锁；ticker 等待移到锁外；`ClockDisable()` 后 ticker nil 解引用（无限阻塞）一并修复。新增并发 `-race` 回归测试 |
| 15 | ✅ | `recording.go`、`dialog_media.go:594` | `AudioStereoRecordingCreate` 按值返回含 `sync.Mutex` 的结构（`go vet` 锁值拷贝告警，拷贝即破坏同步语义）→ 改指针返回 |
| 16 | ⚠️ | `media/media_session.go:297-312` | `Fork()` 丢失 `SecureRTP`/`SRTPAlg`/`remoteProto`/`srtpRemoteTag` → **已修复**（Hold/Unhold/re-INVITE 不再把 SDES 通话降级为明文）。⚠️ 剩余：`filterCodecs`（协商子集）仍不复制——有意保留：外部依赖「覆盖 fork.Codecs 剪枝」的行为（`TestIntegrationDialogServerPeerCodecPruneReinvite` 验证），复制会破坏该用法 |
| 17 | ✅ | `media/rtp_packet_writer.go:98` | 每个 dialog 的 20ms Ticker 永不停止（`ClockDisable()` 全库零调用，`DialogMedia.Close()` 不管它）→ 长期运行 PBX 定时器堆持续增长 → **已修 `2afe5cd`**：`RTPPacketWriter.Close()` 停 ticker 并用 stop channel 唤醒停靠在时钟上的 Write（`ticker.Stop` 不会唤醒接收方），`DialogMedia.Close` 在关 conn 前先释放时钟；废弃的 `ClockDisable` 获得同样唤醒语义 |
| 18 | ⚠️ | `dialog_media.go:151-184` | `initMediaSessionFromConf` 无锁读写 `d.mediaSession`（应答与 BYE/CANCEL 竞态；并发 Answer/ProgressMedia 可泄漏一对 UDP socket）；同族：`PlaybackDTMFCreate`、`AudioReaderDTMF`、`ListenContext`(665-689) 无锁/无 nil 检查读该字段。⚠️ **已修复该家族被 race detector 实测命中的实例**：`Ack()`（dialog_client_session.go:520）无锁读 `mediaSession` 后调 Finalize，与 re-INVITE 的 `replaceRTPSessionUnsafe` 写入竞争 → 改用加锁 getter；`ReadAck` 同步改为锁内取 session（`ead9ff3`）；其余实例待修 |
| 19 | ✅ | `media/media_session.go:1021-1061` | `WriteRTP` 多写者共用单个 marshal 缓冲 → 并发写坏包 → **已修 `7226406`**：改为每呼叫从池取缓冲（稳态零分配），并发写不再互踩 |
| 20 | ❌ | `audio/opus_c.go` | Opus encoder/decoder CGO 状态从不释放（hraban/opus.v2 无 Close/finalizer）→ 每通 opus 通话泄漏一份 C 堆内存 |
| 21 | ⚠️ | `bridge.go:39-130` | `Bridge` 无任何同步（`dialogs` append 与后台代理竞态；第三个 dialog 先 append 后报错；无 Remove/Close）→ **同步与原子双启动已修 `7226406`/`f9e48f7`**；⚠️ 剩余：仍无 Remove/Close 生命周期方法 |
| 22 | ✅ | `bridge.go:495-502` | BridgeMix 非轮询单流路径 `media.ReadAll` 无界累积 → 长通话 OOM → **已修 `f9e48f7`**：改为持续排水丢弃，保持 RTP 流与 RTCP 统计存活 |
| 23 | ❌ | `audio/monitor_pcm.go:203-266` | `MonitorPCMStereo` 未调 `Close` 时临时文件/FD 泄漏（BYE 先到时无兜底） |
| 24 | ✅ | `dialog_media.go:748-756` | `DTMFReader.readDeadline` 用 `StopRTP(1, dur)` 设读截止却 `defer StartRTP(2)` 只恢复写方向 → 读方向永久过期 → **已修 `71fc68d`/后续**：读截止恢复方向改为 `StartRTP(1)`，并在恢复时重新解析当前 media session（re-INVITE 期间替换会话也正确） |
| 25 | ✅ | `dialog_server_session.go:402-426`、`media/media_session.go:656-718` | `d.mu` 持有期间执行 `sess.Finalize()`（DTLS 握手，无超时网络 I/O）→ 丢包场景下阻塞该 dialog 所有媒体操作 → **已修 `ead9ff3`**：锁内完成 SDP 解析，Finalize（DTLS 握手）移到锁外，仍先于 ACK 确认完成，失败即 Hangup |
| 26 | ✅ | `playback_dtmf.go:183-190` | DTMF 读循环每 500ms 覆盖整条连接的读截止时间，破坏其他组件（bridge/ListenBackground）的停止语义 → **已修 `71fc68d`**：连接级 deadline 取消被 pause/interrupt 门整体替换 |

## 三、协议正确性与互通性

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 27 | ✅ | `media/codec.go:19-22`、`media/media_session.go` `updateRemoteCodecs` | 编解码匹配用结构体全等（含硬编码 PT 96/101）→ 浏览器/SBC 的 PT 111 opus 直接 488。**改为按 Name+SampleRate+NumChannels 匹配并采纳远端 PT**；SDP 生成按 Name 输出 rtpmap/fmtp；DTMF 读写器改用协商出的 telephone-event PT（`MediaSession.DTMFCodec()`，未协商回退 101 常量） |
| 28 | ✅ | `media/media_session.go:966` | `err != nil && false` 调试残留吞掉 SRTCP 解密失败 → 密文送 unmarshal、RTCP 静默永久死亡 → 恢复错误返回 + 日志 |
| 29 | ✅ | `media/rtp_session.go:703-739` | RTCP 接收报告：区间零包时伪造 100% 丢包（FractionLost=255）、单包时除零 NaN、序列回绕后区间统计错 65536 倍、`LastSequenceNumber` 恒 cycles=0（TODO）、DLSR/RTT 常数 `65356`（应 65536）、`uint32(min(n, 1<<32))` 恰在 2^32 时截断为 0 → 全部修复（空区间报 0、扩展序列号区间基准、cycles 导出、饱和函数、常数修正并同步修测试） |
| 30 | ✅ | `media/rtp_dtmf_reader.go:65-70` | RFC 4733 明确允许的单包 DTMF 事件（M/E 同置一包）被 `lastEv.Duration == 0` 丢弃；解码失败仍处理零值事件 → **已修 `fc6e04b`/`cdce59e`**：mbit 判定改为 `Marker \|\| lastTimestamp != Timestamp`，单包事件直接交付 |
| 31 | ✅ | `diago.go:371-403` | SIP INFO `application/dtmf-relay` 处理器是永远回 488 的空壳 → **已修 `9829a17`**：实现 Signal=…/Duration=… 解析并派发 OnDTMF |
| 32 | ✅ | `media/sdp/sdp.go:70-75` | 任何含多 m= 行的 SDP（音+视频，很常见）整体拒绝 → **已修 `ca65351`**：接受多 m= SDP，音频 m= 行正常协商（⚠️ 仍不能按 RFC 3264 对不支持媒体回 `m=video 0`，见第七节遗留） |
| 33 | ⚠️ | `media/sdp/sdp.go:124-133` | media-level `c=` 被忽略只读会话级 → **已修 `ca65351`**（WebRTC 式 SDP 的远端地址解析正确，并有 fuzz 覆盖 `551db49`）；⚠️ 剩余：`a=candidate`（ICE）仍完全不解析 |
| 34 | ❌ | `media/media_session.go:496-508` | 对 `UDP/TLS/RTP/SAVP` offer 无 DTLS 配置时回 `RTP/AVP`（RFC 3264 禁止 proto 不一致）；`a=connection:new` 在 re-INVITE 也恒生成（复核 2026-09-07：`media_session.go:1427` 仍无条件 append） |
| 35 | ❌ | `dialog_client_session.go:676-696` | body-less re-INVITE 的 ACK 不带必需的 answer SDP；客户端 late offer（200 无 SDP）当硬错误（相关但不同的服务端 AnswerLate 路径已由 `f9e48f7` 修复并有测试 `9f55156` 覆盖） |
| 36 | ✅ | `media/media_session.go:1021-1025` | `a=inactive` 方向不阻止实际收发 → **已修 `bd934a0`**：ReadRTP/WriteRTP 双门按协商后 `mode` 强制（sendonly/inactive 拒读，recvonly/inactive 拒写），8 组合矩阵测试；⚠️ 静默丢写仍无日志（有意返回成功，调用方无法感知） |
| 37 | ❌ | `dialog_media.go:207-210` | re-INVITE SDP 失败回 487（应 488）；487 保留给 CANCEL（复核 2026-09-07：`dialog_media.go:288` 仍回 487） |
| 38 | ⚠️ | `digest_auth.go:63-130` | 服务端 digest 加固大部分已修：nonce 原子单次使用+过期定时器（`1269416`）、未知/过期 nonce 重新下发 challenge（digest_auth.go:111）、SHA-256 等算法协商 RFC 8760（`45a06ce`）、INVITE 鉴权落地（`9829a17`）；⚠️ 剩余：qop（RFC 2617 完整性）仍未实现 |
| 39 | ❌ | `media/codec.go` + `audio/opus.go:17-34` | 无 `with_opus_c` 构建标签时 opus 仍可被协商成功，首次使用才报错（mid-call 失败）；协商应按能力门控 |
| 40 | ❌ | `dialog_client_session.go:776-801` | Hold 只发 sendonly 不校验对端应答方向，"hold" 状态从不错校（复核 2026-09-07：Hold 实现仍无方向校验） |

## 四、API 设计

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 41 | ✅ | `diago.go` 全文件 | 三套选项风格并存 + SignalOption 双重执行 → **已修 `0177029`**：收敛到 SignalOption 单次执行（契约见 docs/contracts.md §10）；旧 struct options 全部标记 Deprecated 待版本移除 |
| 42 | ⚠️ | `README.md:25-36` | go-version 徽章已指向 fork（`643a2b0`），coverage 徽章对齐实测值（`3fcb7f7`，仍为静态）；⚠️ 剩余：文档站/示例/roadmap 链接仍指上游（有意保留：内容通用）；README 仍宣传 webrtc-pion 分支 |
| 43 | ✅ | `diago.go:292-320` | 生命周期契约完全没有文档 → **已修 `5b74d8f`**：docs/contracts.md §6/§7 落地（handler 返回语义、自动 hangup、Close 幂等），并补 guard |
| 44 | ⚠️ | `diago.go:224`、`register_transaction.go:145`、`bridge.go:424` | `RegisterResponseError` 值/指针接收者已统一（`errors.As` 恢复可用）；⚠️ 剩余：`dg.server, _ =`、`tran, _ :=`（diago.go:296）、builder 错误丢弃等接缝吞错仍在 |
| 45 | ✅ | `playback*.go` | 播放 API 全系无 context、`PlayURL` 写死 10s 超时、哨兵错误与 `io.EOF` 三义判断 → **已修 `71fc68d`**：PlayContext/PlayURLContext 全覆盖，10s 仅在调用方未给 deadline 时兜底；Stop/Pause/Replay 语义改为显式哨兵错误 |
| 46 | ❌ | `dialog_session.go:17-27`、`dialog_media.go:60-65` | 高层接口默认泄漏 sipgo 内部与可变媒体内部，且已被示例依赖收不回来（需 API 版本决策） |
| 47 | ✅ | `dialog_session.go:368-447` | `ReferTransaction` 从未接线（diago.go 走旧的 OnReferDialog 路径）→ **已删除 `b316f92`**（连同其 nil panic 源），Refer 走 handleRefer 既有路径 |
| 48 | ⚠️ | `dialog_cache.go` | in-dialog 成员匹配已暴露 `MatchDialog`（`959dc6e`）；⚠️ 剩余：ctx 忽略（有意为之：sync.Map 后端无阻塞点，调用方无请求级 context）、200 OK 与 ACK 之间窗口仍无法匹配 |

## 五、工程质量与仓库卫生

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 49 | ✅ | `diago.go:834` | `context.WithTimeout` 的 cancel 被丢弃（vet 告警，context 泄漏）→ defer cancel() |
| 50 | ❌ | 全库 | 导出 API 拼写错误成串：`Id()`、"Acknowlededs"、"Temporarly"、"183 Sesion Progress"、`AuidioListen`、`wawWriter`、`allErros`（`65356` 已修）。**决定不做**：全是破坏性改名，应随下一个大版本统一处理 |
| 51 | ✅ | `media/media_session.go:45-47`、`media/dtls.go:21`、`media/logger.go` | 调试全局钩子数据竞争 → 全部改原子（`atomic.Bool`/`atomic.Pointer`），API 微调：赋值 `.Store()`，读取 `.Load()` |
| 52 | ✅ | 测试自身 | 两处测试代码自带数据竞争 → 修复，`go test -race ./...` 全绿 |
| 53 | ⚠️ | 多处 | 死代码清理已完成（`b316f92`：空壳 `dtmf_reader_writer.go`、`getResponse`、注释壳 `AckLate`、死代码 `ReferTransaction`；`ec08f08` 清掉过期 TODO）；服务端 INVITE 鉴权已实现（`9829a17`，opt-in）；⚠️ 剩余：`context.TODO()` 散在 diago.go/dialog_session.go、`WithAuth` 命名占位 |
| 54 | ⚠️ | 测试 | SDP 解析已有 fuzz + 边界回归（`551db49`，290 万次执行无崩溃）；协商/RTCP/digest/DTMF 回归已补；环境依赖测试已门控（`98641b5`）；⚠️ 剩余：register/digest 边界单测仍薄，18 个测试文件仍用 `time.Sleep` |
| 55 | ⚠️ | CI | GitHub Actions vet + race 已落地（`6824002`）并加 gofmt 门禁（`8f2389a`）；⚠️ 剩余：coverage 徽章仍为手写静态（已对齐实测 60.6%，`3fcb7f7`），CI 尚未产出覆盖率 |
| 56 | ✅ | 本机验证发现 | loopback 别名依赖测试已门控（`98641b5`），SIP 测试绑 127.0.0.1（`db62e4a`）；`TestRTPJitterBufferRealtimeSimulation` 时序敏感（80ms 调度余量，任何负载下假失败）→ **已加 `RTP_REALTIME_SIM=1` opt-in 门控 `3ecb738`** |

## 六、已核实无问题的部分（首轮；第二轮复审未推翻）

- RTP jitter buffer（单消费者、close/start once、seq 回绕、SSRC 变更处理正确）
- BridgeMix mixWG Add/Wait 顺序与 pipe 握手（无死锁）；monitor_pcm 竞态重构有效（挂断尾帧 flush 正确）
- `RTPSession` monitor 停止/启动串行化、`d.mu → rtcpMU` 锁序一致
- `DialogMedia.Close`/会话 Close 幂等（CAS）；`playback_dtmf` Close 顺序正确
- offer/answer 方向协商、o= session id/version 递增、SDES 机制（密钥拆分/tag 镜像）、RTCP LSR/DLSR/RTT 结构、RFC 3550 A.8 抖动、A.1 序列跟踪、RFC 2833 DTMF 编码、DTLS-SRTP 密钥推导、REGISTER NAT（rport/received）、关机 unREGISTER、失败协商先 ACK 后 BYE、491 重试随机定时

---

## 七、第二轮复审（2026-09-07，基线 `3fcb7f7`）

复审对象：基线以来 7 个功能性提交（DTLS 指纹、srtp 名字、随机时间戳、writer 时钟、ReadAck Finalize、a=inactive、digest SHA-256）。方法：重读全部 diff + 追入 emiago/dtls v3 源码验证调用路径。**结论：无回归；同时新发现 1 个重要预存缺陷。**

### 本次修复的新发现（已提交）

| 编号 | 位置 | 问题 |
|---|---|---|
| N1 ✅ | `media/dtls.go` `dtlsVerifyConnection`（`dd82db5`） | **DTLS 指纹验证 fail-open**：所有 SDP `a=fingerprint` 都不匹配时落到函数末尾 `return nil`，对端证书与 SDP 声明不符也放行握手，RFC 5763 §5.10 防 MITM 校验完全失效（首轮第六节"DTLS-SRTP 密钥推导已核实"遗漏了这条）。改为 fail-closed；顺带支持 RFC 8122 全部哈希算法（原硬编码只认精确串 `"SHA-256"`，小写 `sha-256` 会被静默跳过=不校验），比较忽略大小写与冒号格式 |
| N2 ✅ | `media/srtp.go` `srtpProfileString`（`7a46a6b`） | `strings.TrimPrefix("SRTP_", p.String())` 参数颠倒，fallback 恒返回 `"SRTP_"` 字面量。修正并加 5 profile 回环测试 |
| N3 ✅ | `media/rtp_packet_writer.go`（`21d7c40`） | RTP 初始时间戳恒为 0（RFC 3550 §5.1 SHOULD 随机），跨呼叫可猜测。随机化；marker 语义保持 |
| N4 ✅ | `media/rtp_jitter_buffer_test.go`（`3ecb738`） | 实时抖动模拟测试对 OS 调度不对称性敏感（抖动上限 400ms vs 480ms 缓冲窗，仅 ~80ms 余量），非 race 下 `go test ./...` 在有负载机器上 5/5 假失败（首轮 #56 只观察到"偶发"，本轮定位根因并 opt-in 门控） |
| N5 ✅ | 集成测试（`4f3186e`） | `ListenPorts` 在 ServeBackground 返回后立即读（注册是异步的）→ `require.Eventually`；re-INVITE 测试遇负载下合法的 491 Request Pending（RFC 3261 §14.2）→ 短重试 |

### 新发现、待修复（按优先级）

| 编号 | 位置 | 问题 | 建议 |
|---|---|---|---|
| N6 ❌ P1 | `media/dtls.go:151` + `media_session.go:750-773` | **DTLS 服务端角色 + 默认 `ServerClientAuthNoCert` 必然握手失败**。diago 为 DTLS 服务端（我方 offer、对端应答 `setup:active`——即 diago 作为主叫走 DTLS 的常见路径）且远端 SDP 带指纹时：emiago/dtls flight5Parse 无条件调用 VerifyConnection，而 NoClientCert 下对端不发证书 → `PeerCertificates` 为空 → 返回错误 → 握手 BadCertificate。该路径测试从未覆盖（`TestDTLSSetup` 用辅助函数传空指纹绕过验证），上游示例也用 NoCert。此为预存缺陷，`dd82db5` 未改变其失败点 | 指纹存在时强制 `ClientAuth=RequireAnyClientCert`（RFC 5763 §5 要求双向出示证书）；补双角色端到端 DTLS-SRTP 媒体测试；发布说明标注"主叫+DTLS 当前不可用（默认配置）" |
| N7 ❌ P2 | `media/dtls.go:94` | 远端 SDP 完全不带 `a=fingerprint` 时静默跳过验证。RFC 5763 会话必须携带指纹；中间人剥离指纹属性即可绕过 N1 的全部保护 | SecureRTP=2 且协商 DTLS 时无指纹应拒绝（至少文档明示降级） |
| N8 ❌ P3 | `media/dtls.go:31` | `ServerClientAuthRequireCert` 实际映射 `RequestClientCert`（请求而非强制）。按名字使用者会误以为服务端会拒绝不出示证书的对端 | 改名或改映射，走废弃周期 |
| N9 ❌ P3 | `media/media_session.go:1197` | WriteRTP 门控注释 "We block here" 与行为不符（实际是静默丢弃并返回成功） | 改注释 |

### `dd82db5` 的两个有意行为变更（发版需写 release notes）

1. 指纹不匹配、或对端仅提供不支持算法（如 md5）的指纹时，现在**中止 DTLS 握手**（旧版放行）。RFC 要求的正确行为，但对极老设备有互通影响。
2. 复合 N6：修复 N6 前，**diago 作为主叫 + DTLS（默认配置）应视为不可用**——旧版在该路径同样握手失败（空证书错误），并非回归。

---

## 修复状态总览（第二轮后）

**首轮 56 项**：✅ 36 项 ｜ ⚠️ 11 项 ｜ ❌ 9 项（#20 opus C 堆泄漏、#23 monitor 临时文件、#34 proto/ connection:new、#35 客户端 late offer、#37 487、#39 opus 门控、#40 Hold 方向、#46 内部泄漏、#50 拼写——#50 决定不做）
**第二轮新增**：✅ 5 项（N1-N5）｜ ❌ 4 项（N6-N9）

**剩余项建议优先级**：
1. **N6/N7 DTLS 服务端角色与指纹强校验**——安全相关且阻塞主叫+DTLS 场景，一个小提交+端到端测试即可关闭；
2. #37（487 应为 488）与 #34（proto 不一致）——协议正确性小修；
3. #21 Bridge Remove/Close、#18 mediaSession 竞态族余下实例——生产稳定性；
4. #39 opus 能力门控、#40 Hold 方向校验——行为完善；
5. #46/#50/#41 余量——API 决策，随下一个大版本。
