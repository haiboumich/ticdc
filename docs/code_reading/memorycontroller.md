# Memory Controller 代码分析

说明：本文档按"上游链路 -> 下游链路 -> 算法对比"的组织方式，详尽体现 memory controller 在上下游各自的作用。

目录:
- [A. 上游 LogPuller Memory Controller](#a-upstream-logpuller)
 - [1 配置与启用](#sec-a1-config)
 - [2 Pause/Resume 机制](#sec-a2-pause-resume)
 - [3 时序图](#sec-a3-sequence)
- [B. 下游 EventCollector Memory Controller](#b-downstream-eventcollector)
 - [1 配额来源与可配置入口](#sec-b1-memory-quota)
 - [2 EventCollector 启用 memory control](#sec-b2-enable-memory-control)
 - [3 changefeed 配额绑定到 AreaSettings](#sec-b3-quota-area)
 - [4 dynstream 把 path 加入 area 并挂上 memControl](#sec-b4-add-path-area)
 - [5 内存统计与控制核心](#sec-b5-core-control)
 - [6 ReleasePath 反馈执行链](#sec-b6-releasepath-flow)
- [C. 两种算法对比](#c-algorithm-comparison)
 - [1 设计理念与角色定位](#sec-c1-design)
 - [2 架构图与时序图](#sec-c2-diagrams)
 - [3 阈值与行为对比](#sec-c3-thresholds)
 - [4 新架构其他变化](#sec-c4-new-arch)
- [D. 术语汇总小节](#d-terminology)

---

<a id="a-upstream-logpuller"></a>
## A. 上游 LogPuller Memory Controller

summary：
- **文档范围**
    - 覆盖上游 [LogPuller](#d-terminology) 的 memory controller 配置与 Pause/Resume 机制。
- **关键组件**
    - [LogPuller](#d-terminology)：从 TiKV 拉取变更数据，使用 [Puller 算法](#d-terminology)。
    - [DynamicStream](#d-terminology)：通用事件处理框架，提供内存控制能力。
- **主数据流**
    - TiKV → LogPuller → DynamicStream → EventStore → EventService
- **核心机制**
    - Pause/Resume：通过 `paused` 标志阻塞/恢复推送，保护数据完整性。
- **设计理念**
    - "宁可慢，不能丢"：数据源头暂停不会丢数据，TiKV 会缓存。

<a id="sec-a1-config"></a>
### 1 配置与启用【业务层】

summary：说明 LogPuller 如何启用 memory controller 及其配置。要点如下：
- LogPuller 在初始化 DynamicStream 时硬编码启用 memory control。
- 使用 [Puller 算法](#d-terminology)（`MemoryControlForPuller`）。
- 内存配额硬编码为 1GB。

#### 1.1 启用 memory control
```golang
// logservice/logpuller/subscription_client.go:251-254
option.UseBuffer = true
option.EnableMemoryControl = true  // 硬编码启用 memory control
ds := dynstream.NewParallelDynamicStream(
    "log-puller",
```

#### 1.2 算法选择
```golang
// logservice/logpuller/subscription_client.go:364
areaSetting := dynstream.NewAreaSettingsWithMaxPendingSize(
    1*1024*1024*1024,                      // 1GB 内存配额（硬编码）
    dynstream.MemoryControlForPuller,      // 硬编码使用 Puller 算法
    "logPuller")
s.ds.AddPath(rt.subID, rt, areaSetting)
```

#### 1.3 配置特点
| 配置项 | 值 | 来源 |
|--------|-----|------|
| EnableMemoryControl | true | 硬编码 |
| 算法类型 | MemoryControlForPuller | 硬编码 |
| 内存配额 | 1GB | 硬编码 |
| 可配置性 | **不可配置** | - |

---

<a id="sec-a2-pause-resume"></a>
### 2 Pause/Resume 机制【业务层 + 基础设施层】

summary：说明 LogPuller 的 Pause/Resume 机制。要点如下：
- LogPuller 维护 `paused: atomic.Bool` 标志。
- `paused=true` 时，`pushRegionEventToDS` 阻塞等待。
- 通过 `cond.Wait()` 和 `cond.Broadcast()` 实现阻塞/唤醒。
- DynamicStream 根据内存使用率发送 PauseArea/ResumeArea feedback。

#### 2.1 LogPuller 的 paused 状态
```golang
// logservice/logpuller/subscription_client.go:193-197
ds dynstream.DynamicStream[int, SubscriptionID, regionEvent, *subscribedSpan, *regionEventHandler]
// the following three fields are used to manage feedback from ds and notify other goroutines
mu     sync.Mutex
cond   *sync.Cond
paused atomic.Bool  // 暂停标志
```

#### 2.2 推送事件时的阻塞逻辑
```golang
// logservice/logpuller/subscription_client.go:399-412
func (s *subscriptionClient) pushRegionEventToDS(subID SubscriptionID, event regionEvent) {
    // fast path: 未暂停时直接推送
    if !s.paused.Load() {
        s.ds.Push(subID, event)
        return
    }
    // slow path: 暂停时等待恢复
    s.mu.Lock()
    for s.paused.Load() {
        s.cond.Wait()  // 阻塞，等待 resume
    }
    s.mu.Unlock()
    s.ds.Push(subID, event)
}
```

#### 2.3 处理 feedback
```golang
// logservice/logpuller/subscription_client.go:419-430
case feedback := <-s.ds.Feedback():
    switch feedback.FeedbackType {
    case dynstream.PauseArea:
        s.paused.Store(true)   // 设置暂停标志
        log.Info("subscription client pause push region event")
    case dynstream.ResumeArea:
        s.paused.Store(false)  // 清除暂停标志
        s.cond.Broadcast()     // 唤醒所有等待的推送
        log.Info("subscription client resume push region event")
    case dynstream.ReleasePath, dynstream.ResumePath:
        // LogPuller 不处理这些 feedback
    }
```

#### 2.4 Puller 算法阈值
```golang
// utils/dynstream/memory_control_algorithm.go:43-76
// PullerMemoryControl 的阈值

// Path 级别
if memoryUsageRatio < 0.1 { ... }   // resume 阈值：10%
if memoryUsageRatio >= 0.2 { ... }  // pause 阈值：20%

// Area 级别
if memoryUsageRatio < 0.5 { ... }   // resume 阈值：50%
if memoryUsageRatio >= 0.8 { ... }  // pause 阈值：80%
```

---

<a id="sec-a3-sequence"></a>
### 3 时序图【完整流程】

```
                                    LogPuller Pause/Resume 流程
                                    ════════════════════════════════

TiKV                         LogPuller                     DynamicStream                   areaMemStat
  │                              │                              │                              │
  │ ──(kv changes)─────────────> │                              │                              │
  │                              │                              │                              │
  │                              │ ──(pushRegionEventToDS)────> │                              │
  │                              │                              │                              │
  │                              │                              │ ──(appendEvent)────────────> │
  │                              │                              │                              │
  │                              │                              │                              │ [统计 pendingSize]
  │                              │                              │                              │ [计算 memoryRatio]
  │                              │                              │                              │
  │                              │                              │ <──(ShouldPauseArea)──────── │
  │                              │                              │   memoryRatio >= 80%?        │
  │                              │                              │                              │
  │                              │ <──(PauseArea feedback)───── │                              │
  │                              │                              │                              │
  │                              │ [paused.Store(true)]         │                              │
  │                              │                              │                              │
  │ ──(kv changes)─────────────> │                              │                              │
  │                              │                              │                              │
  │                              │ ──(pushRegionEventToDS)────> │                              │
  │                              │   if paused.Load():          │                              │
  │                              │     cond.Wait() ←─────────── │─────────────────────────────  │
  │                              │     (阻塞，不推送)            │                              │
  │                              │                              │                              │
  │                              │         ... 等待处理降低内存压力 ...                          │
  │                              │                              │                              │
  │                              │                              │                              │ [处理完成]
  │                              │                              │                              │ [memoryRatio < 50%]
  │                              │                              │                              │
  │                              │ <──(ResumeArea feedback)──── │ <──(ShouldPauseArea)──────── │
  │                              │                              │                              │
  │                              │ [paused.Store(false)]        │                              │
  │                              │ [cond.Broadcast()]           │                              │
  │                              │                              │                              │
  │                              │ ──(恢复推送)───────────────> │                              │
  │                              │                              │                              │
```

**关键特点：**
- **数据完整性保证**：事件保留，只是延迟处理
- **上游缓冲**：TiKV 会缓存未消费的数据
- **阻塞等待**：使用 `cond.Wait()` 阻塞，不丢数据

---

<a id="b-downstream-eventcollector"></a>
## B. 下游 EventCollector Memory Controller

summary：
- **文档范围**
    - 覆盖下游 [EventCollector](#d-terminology) + [DynamicStream](#d-terminology) 的 memory controller 链路。
- **不涉及内容**
    - EventService scan 限流与下游 sink 写入行为。
- **关键组件**
    - [EventCollector](#d-terminology)：内存控制入口与反馈汇聚。
    - [memory controller](#d-terminology)：执行统计、阈值判断与释放策略。
    - [path](#d-terminology)/[area](#d-terminology)：内存统计的最小粒度与分组边界。
- **主数据流**
    - changefeed 配额 -> EventCollector.AddDispatcher -> DynamicStream area/path -> appendEvent -> releaseMemory -> ReleasePath 反馈 -> 清空 path 队列。
- **核心机制**
    - ReleasePath：通过"丢弃/清空"降内存，保护系统稳定性。
- **设计理念**
    - "宁可丢，不能崩"：中间层丢弃后可从上游重新拉取。

**总体架构图：**
```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                              业务层 (Business Layer)                                 │
│  ┌───────────────────────────────────────────────────────────────────────────────┐  │
│  │  Changefeed (复制任务)                                                         │  │
│  │  └── Dispatcher 1, Dispatcher 2, ... Dispatcher N (每个表一个)                  │  │
│  └───────────────────────────────────────────────────────────────────────────────┘  │
│                                        │                                            │
│                                        │ EventCollector 映射                        │
│                                        │   Changefeed → Area                        │
│                                        │   Dispatcher → Path                        │
│                                        ▼                                            │
└─────────────────────────────────────────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                           基础设施层 (Infrastructure Layer)                          │
│  ┌───────────────────────────────────────────────────────────────────────────────┐  │
│  │  DynamicStream (通用事件处理框架)                                               │  │
│  │  ┌─────────────────────────────────────────────────────────────────────────┐  │  │
│  │  │  memControl (内存控制器)                                                  │  │  │
│  │  │    Area (对应 Changefeed)                                                │  │  │
│  │  │    └── Path 1, Path 2, ... Path N (对应 Dispatcher)                      │  │  │
│  │  │        每个 Path 有 pendingQueue + pendingSize                           │  │  │
│  │  └─────────────────────────────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

**关键阈值速查表：**
| 阈值 | 值 | 说明 |
|------|-----|------|
| deadlock 窗口 | 5s | 有入队 && 无出队 |
| deadlock 高水位 | 60% | memoryUsageRatio > 60% |
| 高水位强制释放 | 150% | memoryUsageRatio >= 150% |
| 释放比例 | 40% | totalPendingSize * 40% |
| 释放最小阈值 | 256 bytes | pendingSize >= 256 |

---

<a id="sec-b1-memory-quota"></a>
### 1 配额来源与可配置入口【业务层】

summary：说明 [MemoryQuota](#d-terminology) 的来源（默认值、配置入口）。要点如下：
- MemoryQuota 在 cdc 启动阶段就作为配置项引入/校验。
- 真正生效是在 changefeed 注册到 EventCollector 时。

#### 1.1 默认值
```golang
// pkg/config/server.go:45
DefaultChangefeedMemoryQuota = 1024 * 1024 * 1024 // changefeed 默认内存配额为 1GB
// pkg/config/replica_config.go:47
MemoryQuota: util.AddressOf(uint64(DefaultChangefeedMemoryQuota))
```

#### 1.2 可配置字段
```golang
// pkg/config/replica_config.go:145
MemoryQuota *uint64 `toml:"memory-quota"` // changefeed 的 TOML 配置键名
```

#### 1.3 CLI/配置文件入口
```golang
// cmd/cdc/cli/cli_changefeed_create.go:64
cmd.PersistentFlags().StringVar(&o.configFile, "config", "", "Path of the configuration file")
```

---

<a id="sec-b2-enable-memory-control"></a>
### 2 EventCollector 启用 memory control【业务层】

summary：说明 EventCollector 如何启用 memory controller。要点如下：
- 新架构会启动 EventCollector。
- EventCollector 动态流硬编码启用 EnableMemoryControl。
- 使用 [EventCollector 算法](#d-terminology)（`MemoryControlForEventCollector`）。

#### 2.1 启用 memory control
```golang
// downstreamadapter/eventcollector/helper.go:26-30
option := dynstream.NewOption()
option.UseBuffer = false
option.EnableMemoryControl = true  // 硬编码启用 memory control
if option.EnableMemoryControl {
    log.Info("New EventDynamicStream, memory control is enabled")
```

#### 2.2 算法选择
```golang
// downstreamadapter/eventcollector/event_collector.go:270
areaSetting := dynstream.NewAreaSettingsWithMaxPendingSize(
    memoryQuota,                              // 来自 changefeed 配置
    dynstream.MemoryControlForEventCollector, // 硬编码使用 EventCollector 算法
    "eventCollector")
```

---

<a id="sec-b3-quota-area"></a>
### 3 changefeed 配额绑定到 AreaSettings【业务层 + 基础设施层】

summary：说明在注册 changefeed/dispatcher 时，MemoryQuota 被传入 dynstream。要点如下：
- DispatcherManager 从 changefeed 配置读 MemoryQuota。
- EventCollector.AddDispatcher 把配额传给 dynstream。
- area 的 `maxPendingSize` 直接等于 MemoryQuota。

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager.go:188
sinkQuota: cfConfig.MemoryQuota // DispatcherManager 保存 changefeed 的 memory-quota
// downstreamadapter/dispatchermanager/dispatcher_manager.go:352
...AddDispatcher(..., e.sinkQuota) // 把 quota 传给 EventCollector
// downstreamadapter/eventcollector/event_collector.go:270
areaSetting := dynstream.NewAreaSettingsWithMaxPendingSize(memoryQuota, dynstream.MemoryControlForEventCollector, "eventCollector")
// utils/dynstream/interfaces.go:267
pathMaxPendingSize := max(size/10, 1*1024*1024) // 每个 path 上限=area 的 10%，最少 1MB
// utils/dynstream/interfaces.go:272
maxPendingSize: size // area 的最大待处理内存直接等于 changefeed 配额
```

---

<a id="sec-b4-add-path-area"></a>
### 4 dynstream 把 path 加入 area 并挂上 memControl【基础设施层】

summary：说明 dynstream 内部如何把 path 归入 area，并绑定 memControl。

```golang
// utils/dynstream/parallel_dynamic_stream.go:197
func (s *parallelDynamicStream...) AddPath(...)
// utils/dynstream/parallel_dynamic_stream.go:216
s.setMemControl(pi, as...)
// utils/dynstream/parallel_dynamic_stream.go:277
s.memControl.addPathToArea(pi, setting, s.feedbackChan)
// utils/dynstream/memory_control.go:324
area = newAreaMemStat(path.area, m, settings, feedbackChan)
// utils/dynstream/memory_control.go:328
path.areaMemStat = area
// utils/dynstream/memory_control.go:330
area.pathCount.Add(1)
// utils/dynstream/memory_control.go:332
area.settings.Store(&settings)
```

---

<a id="sec-b5-core-control"></a>
### 5 内存统计与控制核心【基础设施层】

summary：说明事件入队时的内存统计、阈值判定、死锁检测与释放策略。要点如下：
- **releaseMemory 的触发入口**
    - 死锁检测：5s 内有入队 && 无出队 && 内存 > 60%
    - 高水位：内存占用 >= 150%
- **releaseMemory 的执行规则**
    - 按 lastHandleEventTs 降序挑选 blocking path
    - 目标释放量为总 pending 的 40%

**releaseMemory 详细流程：**
```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  Step 1: memory control 分析判断                                                      │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  areaMemStat.releaseMemory()                                                        │
│  │                                                                                  │
│  ├── 1. 计算目标释放量：sizeToRelease = totalPendingSize * 40%                       │
│  │                                                                                  │
│  ├── 2. 按 lastHandleEventTs 降序排序所有 paths                                      │
│  │                                                                                  │
│  ├── 3. 筛选：blocking=true && pendingSize >= 256                                   │
│  │                                                                                  │
│  ├── 4. 选择 paths 直到达到目标释放量                                                 │
│  │                                                                                  │
│  └── 5. 发送 ReleasePath feedback                                                    │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
                                         │
                                         ▼
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  Step 2: EventCollector 接收 feedback（转发者）                                       │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  case feedback := <-c.ds.Feedback():                                                │
│      if feedback.FeedbackType == dynstream.ReleasePath {                            │
│          c.ds.Release(feedback.Path)                                                │
│      }                                                                              │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
                                         │
                                         ▼
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  Step 3: DynamicStream 执行 Release（执行者）                                         │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  DynamicStream.Release(path)                                                        │
│  └── stream.addEvent(release=true, pathInfo)                                        │
│                                                                                     │
│  stream.handleLoop() 收到 release 信号                                               │
│  └── eventQueue.releasePath(pathInfo)                                               │
│      ├── pendingQueue.PopFront() 逐个丢弃事件                                        │
│      ├── areaMemStat.decPendingSize(path, size)                                     │
│      └── path.pendingSize.Store(0)                                                  │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

```golang
// utils/dynstream/memory_control.go:121
if as.checkDeadlock() { as.releaseMemory() } // 检测疑似死锁并触发释放
// utils/dynstream/memory_control.go:125
if as.memoryUsageRatio() >= 1.5 && ... // 内存>150% 且为 EventCollector 算法时强制释放
// utils/dynstream/memory_control.go:36
defaultReleaseMemoryRatio   = 0.4 // 释放比例默认 40%
// utils/dynstream/memory_control.go:37
defaultDeadlockDuration    = 5 * time.Second // 死锁判定窗口为 5 秒
// utils/dynstream/memory_control.go:38
defaultReleaseMemoryThreshold = 256 // 只释放 pendingSize>=256 的阻塞 path
```

---

<a id="sec-b6-releasepath-flow"></a>
### 6 ReleasePath 反馈执行链【基础设施层】

summary：说明 ReleasePath 反馈从 EventCollector 到 dynstream 清空队列的完整链路。

```golang
// downstreamadapter/eventcollector/event_collector.go:423
if feedback.FeedbackType == dynstream.ReleasePath {
// downstreamadapter/eventcollector/event_collector.go:425
c.ds.Release(feedback.Path)
// utils/dynstream/parallel_dynamic_stream.go:189
pi.stream.addEvent(eventWrap...{release: true, pathInfo: pi})
// utils/dynstream/stream.go:211
s.eventQueue.releasePath(e.pathInfo)
// utils/dynstream/event_queue.go:76
_, ok := path.pendingQueue.PopFront()
// utils/dynstream/event_queue.go:83
path.areaMemStat.decPendingSize(path, int64(path.pendingSize.Load()))
// utils/dynstream/event_queue.go:87
path.pendingSize.Store(0)
```

**常见误解澄清：**
| 误解 | 实际 |
|------|------|
| EventCollector 直接清空队列 | ❌ EventCollector 只是转发者 |
| memory control 直接执行释放 | ❌ memory control 只发送 feedback |
| DynamicStream 是被动的 | ❌ DynamicStream 是执行者 |

---

<a id="c-algorithm-comparison"></a>
## C. 两种算法对比

summary：
- **算法定位**
    - 两种算法是 DynamicStream 框架对不同场景的抽象。
    - **不是新老架构的区别**，而是上下游链路的不同角色需求。
- **设计理念**
    - Puller 算法："宁可慢，不能丢" - 数据源头，暂停不丢数据。
    - EventCollector 算法："宁可丢，不能崩" - 中间层，丢弃可重拉。

<a id="sec-c1-design"></a>
### 1 设计理念与角色定位

**算法抽象的来源：**

DynamicStream 是一个**通用事件处理框架**，为不同使用场景提供不同的内存控制策略：

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                              DynamicStream 框架                                      │
│                                                                                     │
│   ┌─────────────────────────────────────────────────────────────────────────────┐   │
│   │                     Memory Controller (内存控制模块)                          │   │
│   │                                                                             │   │
│   │   ┌─────────────────────────────┐   ┌─────────────────────────────────┐   │   │
│   │   │     Puller 算法              │   │     EventCollector 算法          │   │   │
│   │   │     MemoryControlForPuller  │   │     MemoryControlForEventCollector │   │
│   │   ├─────────────────────────────┤   ├─────────────────────────────────┤   │   │
│   │   │                             │   │                                 │   │   │
│   │   │  角色：数据源头               │   │  角色：中间处理层                 │   │   │
│   │   │  场景：上游有缓冲能力          │   │  场景：可从上游重新获取数据        │   │   │
│   │   │  策略：Pause/Resume          │   │  策略：ReleasePath               │   │   │
│   │   │  目标：保护数据完整性          │   │  目标：保护系统稳定性             │   │   │
│   │   │                             │   │                                 │   │   │
│   │   │  使用者：LogPuller           │   │  使用者：EventCollector          │   │   │
│   │   │                             │   │                                 │   │   │
│   │   └─────────────────────────────┘   └─────────────────────────────────┘   │   │
│   │                                                                             │   │
│   └─────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

**为什么需要两种算法？**

| 场景 | 数据源头（上游） | 中间处理层（下游） |
|------|------------------|-------------------|
| 位置 | TiKV → LogPuller | EventService → EventCollector |
| 如果暂停 | TiKV 缓存数据，**不丢** | - |
| 如果丢弃 | - | 可从上游**重新拉取** |
| 优先级 | 数据完整性 | 系统稳定性 |
| 适合算法 | **Puller（Pause/Resume）** | **EventCollector（ReleasePath）** |

---

<a id="sec-c2-diagrams"></a>
### 2 架构图与时序图

**TiCDC 双链路架构：**
```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                    TiCDC 数据流双链路架构                                     │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│  │                          【上游链路 - LogPuller】                                     │   │
│  │                                                                                     │   │
│  │   TiKV ──────> LogPuller ──────> DynamicStream ──────> EventStore ──────> EventService│   │
│  │                 (拉取)            (内存控制)            (存储)            (分发)       │   │
│  │                                      │                                              │   │
│  │                                      │ MemoryControlForPuller                       │   │
│  │                                      │ 算法：Pause/Resume                            │   │
│  │                                      │                                              │   │
│  │                                      │ paused=true: 阻塞推送                         │   │
│  │                                      │ paused=false: 恢复推送                        │   │
│  │                                      │                                              │   │
│  │                                      │ 效果：控制输入速度，保护数据完整性               │   │
│  └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                              │                                              │
│                                              │ 事件流转                                     │
│                                              ▼                                              │
│  ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│  │                         【下游链路 - EventCollector】                                │   │
│  │                                                                                     │   │
│  │   EventService ──────> EventCollector ──────> DynamicStream ──────> Dispatcher      │   │
│  │      (分发)              (路由)               (内存控制)              (写入下游)      │   │
│  │                                                 │                                   │   │
│  │                                                 │ MemoryControlForEventCollector    │   │
│  │                                                 │ 算法：ReleasePath                 │   │
│  │                                                 │                                   │   │
│  │                                                 │ ReleasePath: 清空队列丢弃事件      │   │
│  │                                                 │                                   │   │
│  │                                                 │ 效果：快速释放内存，保护系统稳定性   │   │
│  └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**设计理念对比：**
```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                      设计理念对比                                            │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│                           Puller 算法                          EventCollector 算法          │
│                          ══════════════                        ══════════════════════        │
│                                                                                             │
│   ┌─────────────────────────────────┐                    ┌─────────────────────────────────┐│
│   │                                 │                    │                                 ││
│   │      "宁可慢，不能丢"            │                    │      "宁可丢，不能崩"            ││
│   │                                 │                    │                                 ││
│   │   位置：数据源头                 │                    │   位置：中间处理层               ││
│   │   职责：保护数据完整性           │                    │   职责：保护系统稳定性           ││
│   │   权衡：延迟 vs 完整性           │                    │   权衡：完整性 vs 可用性         ││
│   │                                 │                    │                                 ││
│   └─────────────────────────────────┘                    └─────────────────────────────────┘│
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

---

<a id="sec-c3-thresholds"></a>
### 3 阈值与行为对比

| 维度 | Puller 算法 (上游 LogPuller) | EventCollector 算法 (下游 EventCollector) |
|------|------------------------------|-------------------------------------------|
| **使用位置** | LogPuller | EventCollector |
| **代码引用** | `logservice/logpuller/subscription_client.go:364` | `downstreamadapter/eventcollector/event_collector.go:270` |
| **核心机制** | Pause/Resume (阻塞/恢复) | ReleasePath (丢弃/清空) |
| **内存配额** | 1GB（硬编码） | changefeed 配置（默认 1GB） |
| **Path pause 阈值** | pause: 20%, resume: 10% | 动态计算 |
| **Area pause 阈值** | pause: 80%, resume: 50% | **不触发** |
| **Deadlock 检测** | 无 | 5s 内有入队 && 无出队 |
| **Deadlock 高水位** | 无 | 60% |
| **强制释放** | 无 | 150% |
| **释放比例** | N/A | 40% |
| **数据完整性** | **保证** | **不保证** |
| **OOM 风险** | 较高 | 较低 |

---

<a id="sec-c4-new-arch"></a>
### 4 新架构其他变化

除了 memory controller，新架构还有以下关键变化：

| 变化项 | 老架构 | 新架构 |
|--------|--------|--------|
| **启动方式** | 默认启动 | 需要 `--newarch` 或 `TICDC_NEWARCH=true` |
| **TiKV 版本要求** | 无特殊要求 | 最低 7.5.0 |
| **etcd key 前缀** | `/tidb/cdc/` | `/tidb/cdc_new/` |
| **表数量支持** | ~10万级别 | **100万+** |
| **架构设计** | 单体式 | 云原生、模块化 |

```golang
// 新架构开关
// cmd/cdc/server/server.go:67
cmd.Flags().BoolVarP(&o.serverConfig.Newarch, "newarch", "x", ...)

// TiKV 最低版本要求
// pkg/version/check.go:48-49
MinTiKVVersion = semver.New("7.5.0-alpha")

// etcd key 前缀变化
// pkg/etcd/etcdkey.go:117-118
func NewCDCBaseKey(clusterID string) string {
    return fmt.Sprintf("/tidb/cdc_new/%s", clusterID)
}
```

---

<a id="d-terminology"></a>
## D. 术语汇总小节

- **DynamicStream**：通用事件处理框架，提供事件分发、队列管理、内存控制能力。
    - **本身不处理业务逻辑**，只提供基础设施能力。
    - 包名是 `dynstream`，接口名是 `DynamicStream`。
    - 参考：`utils/dynstream/parallel_dynamic_stream.go:30-46`。

- **Memory Controller**：DynamicStream 的内存控制模块。
    - 内部结构：`memControl`（容器）→ `areaMemStat`（真正干活）
    - 参考：`utils/dynstream/memory_control.go:293`、`utils/dynstream/memory_control.go:43`。

- **Puller 算法**：`MemoryControlForPuller=0`，为数据源头设计的算法。
    - 机制：Pause/Resume（阻塞/恢复）
    - 特点：数据完整性保证，适合上游有缓冲能力的场景
    - 使用者：LogPuller
    - 参考：`utils/dynstream/memory_control_algorithm.go:43-76`。

- **EventCollector 算法**：`MemoryControlForEventCollector=1`，为中间处理层设计的算法。
    - 机制：ReleasePath（丢弃/清空）
    - 特点：快速释放内存，适合可从上游重新获取数据的场景
    - 使用者：EventCollector
    - 参考：`utils/dynstream/memory_control_algorithm.go:159-163`。

- **LogPuller**：上游组件，从 TiKV 拉取变更数据。
    - 使用 Puller 算法 + Pause/Resume 机制
    - 内存配额：1GB（硬编码）
    - 参考：`logservice/logpuller/subscription_client.go`。

- **EventCollector**：下游组件，作为 EventService 与 Dispatcher 之间的中继。
    - **注意：名字有误导性**，实际职责是"路由/分发"
    - 使用 EventCollector 算法 + ReleasePath 机制
    - 内存配额：changefeed 配置
    - 参考：`downstreamadapter/eventcollector/event_collector.go`。

- **Area**：DynamicStream 中的分组概念，用于内存统计。
    - EventCollector 中映射为 ChangefeedID
    - 参考：`utils/dynstream/interfaces.go:26`。

- **Path**：DynamicStream 中的目的端标识，对应一个事件队列。
    - EventCollector 中映射为 DispatcherID
    - 参考：`utils/dynstream/interfaces.go:23`。

- **MemoryQuota**：changefeed 内存配额（字节），默认 1GB。
    - 参考：`pkg/config/server.go:45`、`pkg/config/replica_config.go:47`。

- **ReleasePath**：dynstream 反馈类型，表示释放/丢弃某 path 的队列事件。
    - 参考：`utils/dynstream/interfaces.go:281-289`。

- **PauseArea/ResumeArea**：dynstream 反馈类型，用于 Puller 算法。
    - PauseArea：暂停该 area 的所有事件入队
    - ResumeArea：恢复该 area 的事件入队
    - 参考：`utils/dynstream/interfaces.go`。
