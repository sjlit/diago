# diago 深度代码审查报告

> 审查对象：`github.com/sjlit/diago` @ commit `4c5b8a4`（main 分支）
> 审查方式：三路并行深度审查（架构与 API 设计 / 并发与资源管理 / 协议正确性与工程质量），高严重度条目逐一读源码核实
> 图例：✅ 本次已修复 ｜ ⚠️ 部分修复 ｜ ❌ 待修复（附建议）

---

## 总体评价

媒体底层（jitter buffer、sequencer、DTLS/SDES 密钥协商、SignalOption 层）质量不错，`Diago → DialogSession → DialogMedia` 分层清晰。但库处于**迁移中途状态**：存在可被网络报文直接打挂进程的 panic、必然发生的锁误用与资源泄漏、硬编码导致的真实互通性缺陷，以及三套并存且互相冲突的 API 风格。

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
| 11 | ✅ | `dialog_session.go:439-446` | `ReferTransaction` 构造从不设置 `d` 字段，`Accept()`/`sendNotify()` 一调即 nil panic → 补上（该 API 仍未接线，见四-27） |
| 12 | ✅ | `dialog_client_session.go:781/795/565`、`dialog_server_session.go:717/731/465`、`dialog_client_session.go:768` | 未应答通话调用 `Hold`/`Unhold`/`ReInvite` 直接 nil panic → 统一返回 `"dialog session not answered"`（对齐 bridge.go 既有文案）；`handleReInviteACK` 补 nil 守卫 |
| 13 | ✅ | `diago.go:842-873` | 注册循环对 403/404 等不可重试 4xx 无限重试（无退避上限）→ 4xx（保留 408/491 瞬态码）立即返回错误 |

## 二、并发与资源管理

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 14 | ✅ | `media/rtp_packet_writer.go:153-168` | `Write`/`WriteSamples` 持 **RLock** 修改 `nextTimestamp`/`seqWriter`/`packet`（锁形同虚设）；`lastSampleTime` 锁外裸写 → 改写锁；ticker 等待移到锁外；`ClockDisable()` 后 ticker nil 解引用（无限阻塞）一并修复。新增并发 `-race` 回归测试 |
| 15 | ✅ | `recording.go`、`dialog_media.go:594` | `AudioStereoRecordingCreate` 按值返回含 `sync.Mutex` 的结构（`go vet` 锁值拷贝告警，拷贝即破坏同步语义）→ 改指针返回 |
| 16 | ⚠️ | `media/media_session.go:297-312` | `Fork()` 丢失 `SecureRTP`/`SRTPAlg`/`remoteProto`/`srtpRemoteTag` → **已修复**（Hold/Unhold/re-INVITE 不再把 SDES 通话降级为明文）。⚠️ 剩余：`filterCodecs`（协商子集）仍不复制——有意保留：外部依赖「覆盖 fork.Codecs 剪枝」的行为（`TestIntegrationDialogServerPeerCodecPruneReinvite` 验证），复制会破坏该用法 |
| 17 | ❌ | `media/rtp_packet_writer.go:98` | 每个 dialog 的 20ms Ticker 永不停止（`ClockDisable()` 全库零调用，`DialogMedia.Close()` 不管它）→ 长期运行 PBX 定时器堆持续增长。建议：Close 链上停 ticker |
| 18 | ⚠️ | `dialog_media.go:151-184` | `initMediaSessionFromConf` 无锁读写 `d.mediaSession`（应答与 BYE/CANCEL 竞态；并发 Answer/ProgressMedia 可泄漏一对 UDP socket）；同族：`PlaybackDTMFCreate`、`AudioReaderDTMF`、`ListenContext`(665-689) 无锁/无 nil 检查读该字段。⚠️ **已修复该家族被 race detector 实测命中的实例**：`Ack()`（dialog_client_session.go:520）无锁读 `mediaSession` 后调 Finalize，与 re-INVITE 的 `replaceRTPSessionUnsafe` 写入竞争 → 改用加锁 getter；其余实例待修 |
| 19 | ❌ | `media/media_session.go:1021-1061` | `WriteRTP` 多写者共用单个 marshal 缓冲 → 并发写坏包（配合 #14 修复后暴露面缩小，但 bridge DTMF 直通 + 用户 playback 组合仍可触发） |
| 20 | ❌ | `audio/opus_c.go` | Opus encoder/decoder CGO 状态从不释放（hraban/opus.v2 无 Close/finalizer）→ 每通 opus 通话泄漏一份 C 堆内存 |
| 21 | ❌ | `bridge.go:39-130` | `Bridge` 无任何同步（`dialogs` append 与后台代理竞态；第三个 dialog 先 append 后报错；无 Remove/Close） |
| 22 | ❌ | `bridge.go:495-502` | BridgeMix 非轮询单流路径 `media.ReadAll` 无界累积 → 长通话 OOM |
| 23 | ❌ | `audio/monitor_pcm.go:203-266` | `MonitorPCMStereo` 未调 `Close` 时临时文件/FD 泄漏（BYE 先到时无兜底） |
| 24 | ❌ | `dialog_media.go:748-756` | `DTMFReader.readDeadline` 用 `StopRTP(1, dur)` 设读截止却 `defer StartRTP(2)` 只恢复写方向 → `Listen(dur)` 返回后读方向永久过期，后续读全部超时（应为 `StartRTP(1)`）；`StartRTP(rw, dur)` 公共包装忽略 `dur` 参数 |
| 25 | ❌ | `dialog_server_session.go:402-426`、`media/media_session.go:656-718` | `d.mu` 持有期间执行 `sess.Finalize()`（DTLS 握手，无超时网络 I/O）→ 丢包场景下阻塞该 dialog 所有媒体操作 |
| 26 | ❌ | `playback_dtmf.go:183-190` | DTMF 读循环每 500ms 覆盖整条连接的读截止时间，破坏其他组件（bridge/ListenBackground）的停止语义 |

## 三、协议正确性与互通性

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 27 | ✅ | `media/codec.go:19-22`、`media/media_session.go` `updateRemoteCodecs` | 编解码匹配用结构体全等（含硬编码 PT 96/101）→ 浏览器/SBC 的 PT 111 opus 直接 488。**改为按 Name+SampleRate+NumChannels 匹配并采纳远端 PT**；SDP 生成按 Name 输出 rtpmap/fmtp；DTMF 读写器改用协商出的 telephone-event PT（`MediaSession.DTMFCodec()`，未协商回退 101 常量） |
| 28 | ✅ | `media/media_session.go:966` | `err != nil && false` 调试残留吞掉 SRTCP 解密失败 → 密文送 unmarshal、RTCP 静默永久死亡 → 恢复错误返回 + 日志 |
| 29 | ✅ | `media/rtp_session.go:703-739` | RTCP 接收报告：区间零包时伪造 100% 丢包（FractionLost=255）、单包时除零 NaN、序列回绕后区间统计错 65536 倍、`LastSequenceNumber` 恒 cycles=0（TODO）、DLSR/RTT 常数 `65356`（应 65536）、`uint32(min(n, 1<<32))` 恰在 2^32 时截断为 0 → 全部修复（空区间报 0、扩展序列号区间基准、cycles 导出、饱和函数、常数修正并同步修测试） |
| 30 | ❌ | `media/rtp_dtmf_reader.go:65-70` | RFC 4733 明确允许的单包 DTMF 事件（M/E 同置一包）被 `lastEv.Duration == 0` 丢弃；解码失败仍处理零值事件 |
| 31 | ❌ | `diago.go:371-403` | SIP INFO `application/dtmf-relay` 处理器是永远回 488 的空壳（注释里有格式、解析从未实现），很多 PSTN 网关只靠它传 DTMF |
| 32 | ❌ | `media/sdp/sdp.go:70-75` | 任何含多 m= 行的 SDP（音+视频，很常见）整体拒绝且不能按 RFC 3264 回 `m=video 0` |
| 33 | ❌ | `media/sdp/sdp.go:124-133` | media-level `c=` 被忽略只读会话级 → WebRTC 式 SDP（会话级 0.0.0.0）远端地址解析成 0.0.0.0；`a=candidate`（ICE）完全不解析 |
| 34 | ❌ | `media/media_session.go:496-508` | 对 `UDP/TLS/RTP/SAVP` offer 无 DTLS 配置时回 `RTP/AVP`（RFC 3264 禁止 proto 不一致）；`a=connection:new` 在 re-INVITE 也恒生成 |
| 35 | ❌ | `dialog_client_session.go:676-696` | body-less re-INVITE 的 ACK 不带必需的 answer SDP；客户端 late offer（200 无 SDP）当硬错误（`AckLate` 只剩注释） |
| 36 | ❌ | `media/media_session.go:1021-1025` | `a=inactive` 方向不阻止实际收发；对 recvonly 对端静默丢写无日志 |
| 37 | ❌ | `dialog_media.go:207-210` | re-INVITE SDP 失败回 487（应 488）；487 保留给 CANCEL |
| 38 | ❌ | `digest_auth.go:63-130` | 服务端 digest：nonce 过期后 401 不带新 challenge（客户端无法重认证）、nonce 成功后不删除可重放、仅 MD5 无 qop/SHA-256（RFC 2617/7616） |
| 39 | ❌ | `media/codec.go` + `audio/opus.go:17-34` | 无 `with_opus_c` 构建标签时 opus 仍可被协商成功，首次使用才报错（mid-call 失败）；协商应按能力门控 |
| 40 | ❌ | `dialog_client_session.go:776-801` | Hold 只发 sendnotly 不校验对端应答方向，"hold" 状态从不错校 |

## 四、API 设计

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 41 | ❌ | `diago.go` 全文件 | 三套选项风格并存（7 个已废弃 struct options + functional options + SignalOption）；`InviteOptions.Options()` 与 `NewDialogOptions.Options()` 转换器行为不一致；`Diago.Invite` 中 SignalOption 被执行两次（`InviteBridge` 修了、`Invite` 没同步） |
| 42 | ❌ | `README.md:25-36` | README 宣传 "Answer/Invite 返回媒体栈" 的 API 与 main 分支代码不符（那是另一个分支的事）；徽章/文档站/赞助链接全指向上游 emiago/diago（fork 漂移） |
| 43 | ❌ | `diago.go:292-320` | 框架在 handler 返回后自动 hangup + 硬编码 10s 超时自动关闭 server dialog；`Close` 幂等但 `Close` 后 `Hangup`、hangup 后 `Echo()`/`PlaybackCreate()` 无守卫——生命周期契约完全没有文档 |
| 44 | ❌ | `diago.go:224`、`register_transaction.go:145`、`bridge.go:424` | 公共接缝吞错：`dg.server, _ = sipgo.NewServer(...)`；builder 错误丢弃；`errors.Join` 结果没接；`RegisterResponseError` 值/指针接收者混用致 `errors.As` 半残 |
| 45 | ❌ | `playback*.go` | 播放 API 全系无 context；`PlayURL` 写死 10s 超时；哨兵错误同时匹配 `io.EOF` 造成三义判断；配置靠运行时可变全局；构造器值/指针风格不一（`NewBridge()` 返回值导致 `NewBridge().Add(...)` 编不过） |
| 46 | ❌ | `dialog_session.go:17-27`、`dialog_media.go:60-65` | 高层接口默认泄漏 sipgo 内部（`Do(ctx, *sip.Request)`、`DialogSIP() *sipgo.Dialog`）与可变媒体内部（导出的 RTP reader/writer 字段"只能当只读用"），且已被示例依赖收不回来 |
| 47 | ❌ | `dialog_session.go:368-447` | `ReferTransaction` 从未接线（diago.go 走旧的 OnReferDialog 路径）——本次修复了它的 nil panic，但整条 API 仍是死代码，应删除或接线 |
| 48 | ❌ | `dialog_cache.go` | `DialogCache` 忽略 context、永远返回 nil 的错误；`MatchDialog` 返回三元组 forcing 每个调用方分支；200 OK 与 ACK 之间的 in-dialog 请求窗口无法匹配 |

## 五、工程质量与仓库卫生

| # | 状态 | 位置 | 问题 |
|---|------|------|------|
| 49 | ✅ | `diago.go:834` | `context.WithTimeout` 的 cancel 被丢弃（vet 告警，context 泄漏）→ defer cancel() |
| 50 | ❌ | 全库 | 导出 API 拼写错误成串：`Id()`、"Acknowlededs"、"Temporarly"、"183 Sesion Progress"、`AuidioListen`、`wawWriter`、`allErros`、`65356`（本项已修） |
| 51 | ✅ | `media/media_session.go:45-47`、`media/dtls.go:21`、`media/logger.go` | 调试全局钩子（`RTPDebug`/`RTCPDebug`/`DTLSDebug`/rtpTracer/defLogger）为普通变量，测试翻转时与仍在跑的媒体 goroutine 构成数据竞争（race detector 实证 2 例）→ 全部改原子（`atomic.Bool`/`atomic.Pointer`），**API 微调**：赋值改为 `.Store()`，读取 `.Load()` |
| 52 | ✅ | 测试自身 | 两处测试代码自带数据竞争（`TestMonitorPCMStereo` 两个子测试共享外层 `err` 变量；`TestRTPSessionSourceLockProtection` 写 goroutine 泄漏到下一测试）→ 修复，`go test -race ./...` 从此全绿 |
| 53 | ❌ | 多处 | 死代码：`dtmf_reader_writer.go` 空文件、`getResponse`、重复的 `subState`、仅存注释的 `AckLate`、`sdp/utils.go GenerateForAudio`（自带 bug 且无人用）；30+ TODO 含 `context.TODO()` 入库、服务端 INVITE 鉴权 "TODO authentication"（`WithAuth` 挂着没实现） |
| 54 | ❌ | 测试 | 最高危代码零单测（SDP 解析边界、digest auth、register、DTMF writer、RTCP unmarshal——本次为 SDP/RTCP/协商补了回归测试）；集成测试不门控环境直接绑真实端口；18 个测试文件用 `time.Sleep`；全库无 fuzz test |
| 55 | ❌ | CI | GitLab CI 测试 job `when: manual`、无 GitHub Actions → CI 实际不跑测试；README 61.1% coverage 徽章是手写静态徽章 |
| 56 | ❌ | 本机验证发现 | 以下测试依赖 macOS 缺失的 loopback 别名（Linux CI 可过）：`TestDiagoTransportConfs`(127.0.0.111，**会挂死**)、`TestIntegrationBridging`(127.0.0.200)、`TestIntegrationDialogClientReinviteMedia`(127.0.0.2)、`TestIntegrationPlaybackURL`(127.0.0.100)；`TestRTPJitterBufferRealtimeSimulation` 时间敏感、负载下偶发 |

## 六、已核实无问题的部分（无需复查）

- RTP jitter buffer（单消费者、close/start once、seq 回绕、SSRC 变更处理正确）
- BridgeMix mixWG Add/Wait 顺序与 pipe 握手（无死锁）；近期 monitor_pcm 竞态重构有效（挂断尾帧 flush 正确）
- `RTPSession` monitor 停止/启动串行化、`d.mu → rtcpMU` 锁序一致
- `DialogMedia.Close`/会话 Close 幂等（CAS）；`playback_dtmf` Close 顺序正确
- offer/answer 方向协商、o= session id/version 递增、SDES 机制（密钥拆分/tag 镜像）、RTCP LSR/DLSR/RTT 结构、RFC 3550 A.8 抖动、A.1 序列跟踪、RFC 2833 DTMF 编码、DTLS-SRTP 密钥推导、REGISTER NAT（rport/received）、关机 unREGISTER、失败协商先 ACK 后 BYE、491 重试随机定时

---

## 修复清单（本次交付）

**P0（8 项全修）**：#1-#13 对应表格条目 1-13。
**P1（5 项）**：#14（写锁）、#16（Fork 安全状态）、#27（动态 PT）、#28（SRTCP）、#29（RTCP 统计）。
**额外（5 项）**：#15（锁值拷贝）、#18 部分（Ack 无锁读，race detector 实证）、#49（context 泄漏）、#51（调试全局钩子原子化，API 微调：`RTPDebug` 等改 `atomic.Bool`，赋值用 `.Store()`）、#52（测试自身竞争）。

**新增测试**（全部通过，含 -race）：
- `media/sdp/sdp_edge_test.go`：裸 `\n` 不 panic、末行无 CRLF 不丢失
- `media/negotiation_regression_test.go`：PT 111 opus / PT 100 telephone-event 协商、answer rtpmap/fmtp、`DTMFCodec()`、Fork 安全字段保留
- `media/rtp_session_test.go`：`Monitor()`+`MonitorClose()` 无死锁回归、RTCP 空区间/单包/正常丢包三例
- `media/rtp_packet_writer_test.go`：4 goroutine 并发 Write `-race` 下序列号守恒
- `audio/utils_test.go`：奇数长度混音不 panic、往返一致
- `bridge_test.go`：`unmixStream` 短缓冲不写陈旧字节

**已知未修（按优先级建议）**：
1. #17 Ticker 泄漏、#18 mediaSession 竞态族、#24 readDeadline 方向错误——生产环境影响最大；
2. #30/#31 DTMF 互通、#32/#33 SDP 多 m= 行与 media-level c=——互通性；
3. #38 digest 加固、#40-#48 API 收敛与文档——需要 API 设计决策，建议先在 Issue 里定契约再动。
