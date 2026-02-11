# Memory Controller 报告（代码验证版）

说明：以下按“入口 -> 逻辑 -> 最底层释放”的链路组织；每条记录包含 `文件:行号`、代码片段、说明。

目录：
- A. Memory Controller 运行机制详解（入口 -> 逻辑 -> 释放）
  - 1 配额来源与可配置入口
  - 2 新架构入口：EventCollector 启用 memory control
  - 3 changefeed 配额绑定到 AreaSettings（changefeed -> dispatcher -> dynstream）
  - 4 dynstream 把 path 加入 area 并挂上 memControl
  - 5 内存统计与控制核心（append/ratio/释放）
  - 6 ReleasePath 反馈执行链（从入口到最底层）
  - 7 Pause/Resume 逻辑现状（新架构 vs 老架构）
- B. 参考章节：新架构数据流上层逻辑（代码验证版）
  - 8 新架构数据流上层逻辑（代码验证版）

## A. Memory Controller 运行机制详解（入口 -> 逻辑 -> 释放）

### 1 配额来源与可配置入口

summary：说明 MemoryQuota 的来源（默认值、配置入口）与 dynstream 兜底默认值。MemoryQuota 在 cdc 启动阶段就作为配置项引入/校验，但真正生效（作为 area 的上限）是在 changefeed 注册到 EventCollector 时（见第 3 节）。

#### 1.1 默认值
```golang
// pkg/config/server.go:45
DefaultChangefeedMemoryQuota = 1024 * 1024 * 1024 // changefeed 默认内存配额为 1GB
// pkg/config/replica_config.go:47
MemoryQuota: util.AddressOf(uint64(DefaultChangefeedMemoryQuota)) // ReplicaConfig 默认把 MemoryQuota 设为 1GB
```

#### 1.2 可配置字段
```golang
// pkg/config/replica_config.go:145
MemoryQuota *uint64 `toml:"memory-quota"` // changefeed 的 TOML 配置键名为 memory-quota
// pkg/config/changefeed.go:193
MemoryQuota uint64 `toml:"memory-quota"` // ChangefeedConfig 内部保存内存配额（单位字节）
```

#### 1.3 CLI/配置文件入口
```golang
// cmd/cdc/cli/cli_changefeed_create.go:64
cmd.PersistentFlags().StringVar(&o.configFile, "config", "", "Path of the configuration file") // 创建 changefeed 时通过 --config 指定配置文件
```

#### 1.4 仓库内示例（TOML 片段）
```golang
// cmd/config-converter/main_test.go:39
memory-quota = 100 // 测试用 TOML 示例，展示键名与格式
```

#### 1.5 dynstream 默认兜底（当 AreaSettings 未设置或 size<=0）
```golang
// utils/dynstream/interfaces.go:203
DefaultMaxPendingSize = uint64(1024 * 1024 * 1024) // dynstream 默认的最大待处理内存 1GB
// utils/dynstream/interfaces.go:257
s.maxPendingSize = DefaultMaxPendingSize // AreaSettings.fix() 在 size<=0 时回退到默认值
```

---

### 2 新架构入口：EventCollector 启用 memory control

summary：说明“是否必然启用 memory controller”的判断链路：newarch=true 时必然启动 EventCollector，且 dynstream 的 EnableMemoryControl 被硬编码打开。

调用链：
- 新架构开关（newarch）
  - newarch=true 时进入新架构 server
    - setPreServices 创建 EventCollector
      - EventCollector 动态流启用 EnableMemoryControl

重要结论：**新架构必然启用 memory controller**（EventCollector 动态流硬编码 `EnableMemoryControl=true`）。

#### 2.1 新架构开关与入口（是否必然启用 EventCollector）
```golang
// pkg/config/server.go:91
Newarch:       false // newarch 默认关闭
// cmd/cdc/server/server.go:67
cmd.Flags().BoolVarP(&o.serverConfig.Newarch, "newarch", "x", o.serverConfig.Newarch, "Run the new architecture of TiCDC server") // CLI 开关
// cmd/cdc/server/server.go:301
newarch = os.Getenv("TICDC_NEWARCH") == "true" // 环境变量开关
// cmd/cdc/server/server.go:281
newarch = isNewArchEnabledByConfig(serverConfigFilePath) // 读取 server 配置文件的 newarch
// cmd/cdc/server/server.go:368
if isNewArchEnabled(o) { // newarch=true -> 新架构
// cmd/cdc/server/server.go:378
err = o.run(cmd) // 进入新架构 server 运行路径
// cmd/cdc/server/server.go:382
return runTiFlowServer(o, cmd) // newarch=false -> 旧架构路径
```

#### 2.2 新架构下 EventCollector 与 memory control（是否必然启用）
```golang
// server/server.go:259
ec := eventcollector.New(c.info.ID) // 新架构 preServices 中创建 EventCollector
// server/server.go:261
ec.Run(ctx) // 启动 EventCollector
// downstreamadapter/eventcollector/helper.go:26
option := dynstream.NewOption() // 创建 dynstream 运行参数
// downstreamadapter/eventcollector/helper.go:30
option.EnableMemoryControl = true // EventCollector 动态流硬编码开启 memory control
// utils/dynstream/interfaces.go:217
EnableMemoryControl bool // memory control 默认关闭，需显式开启
// utils/dynstream/parallel_dynamic_stream.go:72
if option.EnableMemoryControl { // 判断是否启用 memory control
// utils/dynstream/parallel_dynamic_stream.go:74
s.feedbackChan = make(chan Feedback[A, P, D], 1024) // 创建内存控制反馈通道
// utils/dynstream/parallel_dynamic_stream.go:75
s.memControl = newMemControl[A, P, T, D, H]() // 初始化内存控制器实例
```

---

### 3 changefeed 配额绑定到 AreaSettings（changefeed -> dispatcher -> dynstream）

summary：说明在注册 changefeed/dispatcher 时，MemoryQuota 被传入 dynstream，成为 area 的 `maxPendingSize`（内存上限），并派生 path 的默认上限。

调用链：
- changefeed 配置
  - DispatcherManager.sinkQuota
    - EventCollector.AddDispatcher
      - dynstream.NewAreaSettingsWithMaxPendingSize

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager.go:188
sinkQuota: cfConfig.MemoryQuota // DispatcherManager 保存 changefeed 的 memory-quota
// downstreamadapter/dispatchermanager/dispatcher_manager.go:352
...AddDispatcher(..., e.sinkQuota) // 把 quota 传给 EventCollector
// downstreamadapter/eventcollector/event_collector.go:270
areaSetting := dynstream.NewAreaSettingsWithMaxPendingSize(memoryQuota, dynstream.MemoryControlForEventCollector, "eventCollector") // 用 quota 创建 area 设置并指定算法
// utils/dynstream/interfaces.go:267
pathMaxPendingSize := max(size/10, 1*1024*1024) // 每个 path 上限=area 的 10%，最少 1MB
// utils/dynstream/interfaces.go:272
maxPendingSize: size // area 的最大待处理内存直接等于 changefeed 配额
// utils/dynstream/interfaces.go:274
algorithm: memoryControlAlgorithm // 记录使用的内存控制算法类型
```

---

### 4 dynstream 把 path 加入 area 并挂上 memControl

summary：说明 dynstream 内部如何把 path 归入 area，并绑定 memControl 以建立统计与反馈机制。

调用链：
- DynamicStream.AddPath
  - setMemControl
    - memControl.addPathToArea
      - newAreaMemStat / 绑定 settings

```golang
// utils/dynstream/parallel_dynamic_stream.go:197
func (s *parallelDynamicStream...) AddPath(...) // 动态流注册新 path 的入口
// utils/dynstream/parallel_dynamic_stream.go:216
s.setMemControl(pi, as...) // 将 path 与 memControl 绑定
// utils/dynstream/parallel_dynamic_stream.go:277
s.memControl.addPathToArea(pi, setting, s.feedbackChan) // path 加入 area 并建立反馈通道
// utils/dynstream/memory_control.go:324
area = newAreaMemStat(path.area, m, settings, feedbackChan) // 创建 area 级内存统计
// utils/dynstream/memory_control.go:328
path.areaMemStat = area // path 持有 area 的内存统计引用
// utils/dynstream/memory_control.go:330
area.pathCount.Add(1) // 记录 area 内 path 数量
// utils/dynstream/memory_control.go:332
area.settings.Store(&settings) // 保存 area 的内存上限与算法设置
```

---

### 5 内存统计与控制核心（append/ratio/释放）

summary：说明事件入队时的内存统计、阈值判定、死锁检测与释放策略（核心控制逻辑）。

调用链：
- path.appendEvent
  - areaMemStat.appendEvent
    - 统计 size / 判定 deadlock / 触发 release

```golang
// utils/dynstream/stream.go:370
func (pi *pathInfo...) appendEvent(...) bool // path 收到事件进入内存控制链路的入口
// utils/dynstream/stream.go:372
return pi.areaMemStat.appendEvent(pi, event, handler) // 事件交给 areaMemStat 做统一计量与控制
// utils/dynstream/memory_control.go:101
defer as.updateAreaPauseState(path) // 事件追加后更新 area 状态（新架构不触发暂停）
// utils/dynstream/memory_control.go:121
if as.checkDeadlock() { as.releaseMemory() } // 检测疑似死锁并触发释放
// utils/dynstream/memory_control.go:125
if as.memoryUsageRatio() >= 1.5 && ... // 内存>150% 且为 EventCollector 算法时强制释放
// utils/dynstream/memory_control.go:128
if event.eventType.Droppable { ... handler.OnDrop(...) } // 可丢弃事件会转换成 drop 事件
// utils/dynstream/memory_control.go:152
path.pendingQueue.PushBack(event) // 事件入队列
// utils/dynstream/memory_control.go:154
path.updatePendingSize(int64(event.eventSize)) // 更新 path 级待处理字节数
// utils/dynstream/memory_control.go:155
as.totalPendingSize.Add(int64(event.eventSize)) // 更新 area 级待处理字节总量
// utils/dynstream/memory_control.go:221
return float64(as.totalPendingSize.Load()) / float64(as.settings.Load().maxPendingSize) // 计算 area 内存占用比例
// utils/dynstream/memory_control.go:36
defaultReleaseMemoryRatio     = 0.4 // 释放比例默认 40%
// utils/dynstream/memory_control.go:37
defaultDeadlockDuration       = 5 * time.Second // 死锁判定窗口为 5 秒
// utils/dynstream/memory_control.go:38
defaultReleaseMemoryThreshold = 256 // 只释放 pendingSize>=256 的阻塞路径
// utils/dynstream/memory_control.go:167
hasEventComeButNotOut := ... // 死锁判定：有输入但无输出
// utils/dynstream/memory_control.go:169
memoryHighWaterMark := as.memoryUsageRatio() > (1 - defaultReleaseMemoryRatio) // 高水位阈值=60%
// utils/dynstream/memory_control.go:191
sizeToRelease := int64(float64(as.totalPendingSize.Load()) * defaultReleaseMemoryRatio) // 计算需要释放的总量
// utils/dynstream/memory_control.go:198
if ... !path.blocking.Load() { continue } // 只选择阻塞路径进行释放
// utils/dynstream/memory_control.go:211
FeedbackType: ReleasePath // 发送 ReleasePath 反馈
```

---

### 6 ReleasePath 反馈执行链（从入口到最底层）

summary：说明 ReleasePath 反馈从 EventCollector 下发到 dynstream 清空队列的完整执行路径。

调用链：
- EventCollector 接收 ReleasePath
  - dynstream.Release
    - stream 收到 release 信号
      - eventQueue 清空 path

```golang
// downstreamadapter/eventcollector/event_collector.go:423
if feedback.FeedbackType == dynstream.ReleasePath { // EventCollector 接收并处理 ReleasePath 反馈
// downstreamadapter/eventcollector/event_collector.go:425
c.ds.Release(feedback.Path) // 调用 dynstream 释放该 path
// utils/dynstream/parallel_dynamic_stream.go:189
pi.stream.addEvent(eventWrap...{release: true, pathInfo: pi}) // 将 release 信号投递到具体 stream
// utils/dynstream/stream.go:211
s.eventQueue.releasePath(e.pathInfo) // handleLoop 收到 release 信号后清空该 path
// utils/dynstream/event_queue.go:76
_, ok := path.pendingQueue.PopFront() // 逐个弹出并丢弃待处理事件
// utils/dynstream/event_queue.go:83
path.areaMemStat.decPendingSize(path, int64(path.pendingSize.Load())) // 释放后扣减 area 的内存统计
// utils/dynstream/event_queue.go:87
path.pendingSize.Store(0) // 将 path 待处理内存清零
// utils/dynstream/memory_control.go:283
as.totalPendingSize.Add(int64(-size)) // decPendingSize 同步扣减 area 总量
```

---

### 7 Pause/Resume 逻辑现状（新架构 vs 老架构）

summary：对比新架构（EventCollector）与旧架构（Puller）的 pause/resume 行为与阈值差异。

调用链：
- areaMemStat.updateAreaPauseState
  - algorithm.ShouldPauseArea
    - EventCollector: 不触发 area pause/resume
    - Puller: 固定阈值 20/10 与 80/50

```golang
// utils/dynstream/memory_control.go:226
pause, resume, memoryUsageRatio := as.algorithm.ShouldPauseArea(...) // memory_control 当前只计算 area 级 pause/resume
// utils/dynstream/memory_control_algorithm.go:160
return false, false, memoryUsageRatio // EventCollector 算法不触发 area pause/resume
// utils/dynstream/memory_control.go:278
if ... MemoryControlForEventCollector && as.paused.Load() { log.Panic(...) } // 保护：EventCollector 不应进入 area paused
// utils/dynstream/memory_control_algorithm.go:43
if memoryUsageRatio < 0.1 { ... } // Puller 算法的 path resume 阈值（10%）
// utils/dynstream/memory_control_algorithm.go:52
if memoryUsageRatio >= 0.2 { ... } // Puller 算法的 path pause 阈值（20%）
// utils/dynstream/memory_control_algorithm.go:66
if memoryUsageRatio < 0.5 { ... } // Puller 算法的 area resume 阈值（50%）
// utils/dynstream/memory_control_algorithm.go:70
if memoryUsageRatio >= 0.8 { ... } // Puller 算法的 area pause 阈值（80%）
```

## B. 参考章节：新架构数据流上层逻辑（代码验证版）

### 8 新架构数据流上层逻辑（代码验证版）

summary：给出上层数据流（Puller / Sinker）组件分工的代码验证参考，不涉及 memory controller 的细节实现。

#### 8.1 上游获取侧（Puller 责任链）
```golang
// server/server.go:188
subscriptionClient := logpuller.NewSubscriptionClient(...) // 初始化 LogPuller/SubscriptionClient，负责订阅上游日志
// server/server.go:195
eventStore := eventstore.New(conf.DataDir, subscriptionClient) // EventStore 依赖 SubscriptionClient，承接上游事件落地与管理
// logservice/eventstore/event_store.go:31
"github.com/pingcap/ticdc/logservice/logpuller" // EventStore 直接引用 logpuller，证明其上游拉取依赖
// server/server.go:196
eventService := eventservice.New(eventStore, schemaStore) // EventService 以 EventStore 为数据源对外提供拉取服务
// pkg/eventservice/event_service.go:73
eventStore  eventstore.EventStore // EventService 内部持有 EventStore，作为事件来源
```

#### 8.2 下游写入侧（Sinker 责任链）
```golang
// server/server.go:259
ec := eventcollector.New(c.info.ID) // 启动 EventCollector，作为下游处理入口
// downstreamadapter/dispatchermanager/dispatcher_manager.go:218
manager.sink, err = sink.New(...) // 每个 changefeed 的 DispatcherManager 初始化 Sink（下游写入端）
// downstreamadapter/dispatchermanager/dispatcher_manager.go:439
d := dispatcher.NewEventDispatcher(...) // 为每个表/Span 创建 EventDispatcher，负责驱动下游写入
// downstreamadapter/dispatcher/event_dispatcher.go:34
// EventDispatcher is the dispatcher to flush events to the downstream // EventDispatcher 明确是“写下游”的角色
```

#### 8.3 组件编排（并不完全属于拉/写任一侧）
```golang
// server/server.go:112
preServices ... [PDClock, MessageCenter, EventCollector, HeartbeatCollector, DispatcherOrchestrator, KeyspaceManager] // 下游入口与编排组件在 preServices 中启动
// server/server.go:134
subModules ... [SubscriptionClient, SchemaStore, MaintainerManager, EventStore, EventService] // 上游拉取与元信息模块在 subModules 中启动
```
