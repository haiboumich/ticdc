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
  - [4 常见问题解答](#sec-c4-faq)
  - [5 新架构其他变化](#sec-c5-new-arch)
- [D. 术语汇总小节](#d-terminology)

---

## TiCDC 整体架构与数据流

**"上下游"是数据流方向，不是物理部署**。LogPuller 和 EventCollector 都在同一个 TiCDC 进程中，但在数据流中处于不同位置。

### 架构概览

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                     TiCDC 整体架构与数据流                                    │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   ┌──────────┐                                                                           │ │
│   │          │                                                                           │ │
│   │   TiKV   │  数据源：存储变更数据，有自身缓冲能力                                          │ │
│   │          │                                                                           │ │
│   └────┬─────┘                                                                           │ │
│        │                                                                                 │ │
│        │ ① gRPC Stream（LogPuller 主动订阅 Pull 模式）                                      │ │
│        ▼                                                                                 │ │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐  │ │
│   │                         TiCDC 进程（进程内单例）                                      │  │ │
│   │                                                                                     │  │ │
│   │  ┌───────────────────────────────────────────────────────────────────────────────┐  │  │ │
│   │  │                        上游链路（数据入口）                                       │  │  │ │
│   │  │                                                                               │  │  │ │
│   │  │   LogPuller ──────> DynamicStream ──────> EventStore                          │  │  │ │
│   │  │      │                   │                        │                          │  │  │ │
│   │  │      │                   │                        │                          │  │  │ │
│   │  │      │         MemoryControlForPuller            │                          │  │  │ │
│   │  │      │         算法：Pause/Resume                 │                          │  │  │ │
│   │  │      │                   │                        │                          │  │  │ │
│   │  │      │<───── feedback ───┘                        │                          │  │  │ │
│   │  │                                                     │                          │  │  │ │
│   │  │                                          PebbleDB 持久化存储                      │  │  │ │
│   │  │                                                     │                          │  │  │ │
│   │  └─────────────────────────────────────────────────────┼──────────────────────────┘  │  │
│   │                                                        │                              │  │
│   │                                                        │                              │  │
│   │  ┌─────────────────────────────────────────────────────┼──────────────────────────┐  │  │
│   │  │                        EventService（服务层）          │                          │  │  │
│   │  │                                                    │                          │  │  │ │
│   │  │  • 持有 EventStore 引用                             │                          │  │  │ │
│   │  │  • 从 EventStore 扫描事件                           │                          │  │  │ │
│   │  │  • 主动推送给 EventCollector                        │                          │  │  │ │
│   │  │                                                    ▼                          │  │  │ │
│   │  │                          ② Messaging（Push 模式）                               │  │  │ │
│   │  │                                                    │                          │  │  │ │
│   │  └────────────────────────────────────────────────────┼──────────────────────────┘  │  │
│   │                                                       │                              │  │
│   │                                                       ▼                              │  │
│   │  ┌───────────────────────────────────────────────────────────────────────────────┐  │  │ │
│   │  │                        下游链路（数据出口）                                       │  │  │ │
│   │  │                                                                               │  │  │ │
│   │  │   EventCollector ──────> DynamicStream ──────> Dispatcher ───> Sink           │  │  │ │
│   │  │        │                        │                                          │  │  │ │
│   │  │        │                        │                                          │  │  │ │
│   │  │        │              MemoryControlForEventCollector                        │  │  │ │
│   │  │        │              算法：ReleasePath                                       │  │  │ │
│   │  │        │                        │                                          │  │  │ │
│   │  │        │              <───── feedback ────                                   │  │  │ │
│   │  │        │                     (释放队列)                                       │  │  │ │
│   │  │                                                                               │  │  │ │
│   │  └───────────────────────────────────────────────────────────────────────────────┘  │  │
│   │                                                                                     │  │
│   └─────────────────────────────────────────────────────────────────────────────────────┘  │
│        │                                                                                 │ │
│        │ ③ 写入下游                                                                       │ │
│        ▼                                                                                 │ │
│   ┌──────────┐                                                                           │ │
│   │          │                                                                           │ │
│   │  MySQL/  │  数据目的地：接收变更数据                                                    │ │
│   │  Kafka   │                                                                           │ │
│   │          │                                                                           │ │
│   └──────────┘                                                                           │ │
│                                                                                           │ │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 数据流模式说明

| 数据流段 | 模式 | 说明 |
|---------|------|------|
| ① TiKV → LogPuller | **Pull（拉取）** | LogPuller 通过 gRPC 主动订阅 TiKV 变更 |
| ② EventService → EventCollector | **Push（推送）** | EventService 通过 Messaging 主动推送事件 |
| ③ Dispatcher → Sink | **Push（推送）** | Dispatcher 主动写入下游（MySQL/Kafka/…） |

### 组件关系说明

| 组件 | 职责 | 关系 |
|------|------|------|
| **LogPuller** | 从 TiKV 拉取变更数据 | 通过 gRPC Stream 订阅 TiKV |
| **EventStore** | 持久化存储变更数据 | 使用 PebbleDB 存储，被 EventService 持有 |
| **EventService** | 事件分发服务（进程内单例） | 持有 EventStore，扫描并推送事件给 EventCollector |
| **EventCollector** | 接收并路由事件 | 接收 EventService 推送，路由给 Dispatcher（每个 changefeed 一个实例） |
| **Dispatcher** | 处理事件并写入下游 | 通过 Sink 写入 MySQL/Kafka，**每个表对应一个 Dispatcher** |

### 组件映射关系（关键概念）

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                              TiCDC 组件映射关系与作用域                                      │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   组件                    │       作用域               │       对应关系                      │
│   ────────────────────────────────────────────────────────────────────────────────────────  │
│   LogPuller              │   订阅级别 (subscriptionID)   │   一个订阅 → 多个 region        │
│   EventStore             │   进程内单例                 │   全局唯一的持久化存储           │
│   EventService           │   进程内单例                 │   全局唯一的分发服务            │
│   EventCollector         │   changefeed 级别            │   一个 changefeed → 一个实例     │
│   DynamicStream(下游)    │   changefeed 级别 (Area)     │   一个 changefeed → 一个 Area    │
│   Dispatcher            │   table 级别 (Path)         │   每个表 → 一个 Dispatcher       │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**层级关系示意图：**

```
TiCDC 进程
│
├── [单例] EventStore          ← 存储所有 changefeed 的数据
│                              │
├── [单例] EventService        ← 统一分发事件给所有 changefeed
│                              │
└── 多个 Changefeed 实例
    │
    ├── Changefeed A (包含多个表)
    │   │                        → EventCollector A → Area A (memory control)
    │   ├── Dispatcher A1 (表1) → Path A1
    │   ├── Dispatcher A2 (表2) → Path A2
    │   └── Dispatcher A3 (表3) → Path A3
    │
    ├── Changefeed B (包含多个表)
    │   │                        → EventCollector B → Area B (memory control)
    │   ├── Dispatcher B1 (表4) → Path B1
    │   └── Dispatcher B2 (表5) → Path B2
    │
    └── Changefeed C (包含多个表)
        │                        → EventCollector C → Area C (memory control)
        ├── Dispatcher C1 (表6) → Path C1
        └── Dispatcher C2 (表7) → Path C2
```

**关键要点：**
1. **EventStore、EventService 是进程内单例**：服务于所有 changefeed
2. **LogPuller 按订阅组织**：一个订阅可能包含多个 region，但不直接对应 changefeed
3. **EventCollector 按 changefeed 划分**：每个 changefeed 有独立的 EventCollector 实例
4. **Dispatcher 是表级别的**：一个 changefeed 包含多个表，每个表对应一个 Dispatcher
5. **下游 memory control 的 Area/Path 映射**：Area → Changefeed，Path → Dispatcher (每个表一个 Path)

### 关键设计决策

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                     为什么这样设计？                                          │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│  1. LogPuller 用 Pull 模式（而不是 TiKV Push）                                               │
│     ─────────────────────────────────────────                                                │
│     • TiKV 不需要知道 TiCDC 的存在，解耦                                                      │
│     • TiCDC 可以控制拉取速度，实现流量控制（Memory Controller）                                │
│     • TiKV 有 gRPC CDC 接口，客户端主动订阅                                                   │
│                                                                                             │
│  2. EventService 用 Push 模式（而不是 EventCollector Pull）                                  │
│     ─────────────────────────────────────────────────────                                    │
│     • EventService 知道哪些 Dispatcher 需要哪些数据                                           │
│     • 减少轮询开销，事件就绪时主动推送                                                         │
│     • 通过 Messaging 组件实现跨节点通信                                                        │
│                                                                                             │
│  3. EventStore 持久化存储（而不是纯内存）                                                     │
│     ─────────────────────────────────────────                                                │
│     • 支持重新拉取：Dispatcher 丢弃后可以从 EventStore 重新获取                               │
│     • 支持 resolvedTs 机制：追踪已处理进度                                                    │
│     • 数据安全：即使 EventCollector 崩溃也能恢复                                               │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 关键点

┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│  1. LogPuller 靠近数据源：如果暂停 → TiKV 会在 GC safepoint 时间窗口内缓存数据                 │
│     （需配合 tikv_gc_life_time 配置，默认 10m；超时可能丢失）                                   │
│  2. EventCollector 靠近数据目的地：如果丢弃 → 可从 EventStore 重新拉取                         │
│     （前提：EventStore 数据未被 GC，且上游 TiKV 数据仍在 safepoint 内）                         │
│  3. 两者都在同一进程，但在数据流中位置不同，面对的约束不同，所以用不同策略                        │
│  4. EventService 和 EventStore 均为进程内单例，统一管理该进程的事件分发                         │
└─────────────────────────────────────────────────────────────────────────────────────────────┘

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
// PullerMemoryControl 的阈值定义

// Path 级别（定义于 ShouldPausePath，但运行时未调用）
if memoryUsageRatio < 0.1 { ... }   // resume 阈值：10%
if memoryUsageRatio >= 0.2 { ... }  // pause 阈值：20%

// Area 级别（实际运行时使用的阈值）
// 参见 utils/dynstream/memory_control.go:226 - updateAreaPauseState 只调用 ShouldPauseArea
if memoryUsageRatio < 0.5 { ... }   // resume 阈值：50%
if memoryUsageRatio >= 0.8 { ... }  // pause 阈值：80%
```

> ⚠️ **注意**：`ShouldPausePath` 方法定义在算法接口中，但当前运行时逻辑（`memory_control.go:226`）只调用 `ShouldPauseArea`。
> Path 级别阈值存在于算法定义中，但**实际 pause/resume 决策仅基于 Area 级别**。

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
| **Path pause 阈值** | 20%/10%（定义存在，**未生效** ⚠️） | 动态计算（定义存在，**未生效** ⚠️） |
| **Area pause 阈值** | pause: 80%, resume: 50% ✅ | **不触发**（始终返回 false） |
| **Deadlock 检测** | 无 | 5s 内有入队 && 无出队 |
| **Deadlock 高水位** | 无 | 60% |
| **强制释放** | 无 | 150% |
| **释放比例** | N/A | 40% |
| **数据完整性** | **保证**（GC safepoint 内） | **不保证**（可从上游恢复） |
| **OOM 风险** | 中等（依赖消费者正常工作） | 较低（可快速释放） |

> ⚠️ **Path pause 阈值说明**：Puller 算法和 EventCollector 算法的 Path 级别阈值都定义于 `ShouldPausePath` 接口，但**运行时实际只调用 `ShouldPauseArea`**（参见 `memory_control.go:226`）。因此 Path 级别的暂停/恢复机制**当前未生效**。
>
> 💡 **运行时实际情况**：当前只有 Puller 算法的 Area pause 阈值（80%/50%）在实际生效。EventCollector 的 `ShouldPauseArea` 始终返回 false，不触发暂停。因此**当前没有实现 Path 级别的渐进式控制**（即先暂停部分 path，再暂停整个 area）。

---

<a id="sec-c4-faq"></a>
### 4 常见问题解答

#### Q1: Puller 算法会不会永远 hang 住？

**问题**：假设 area 超过 80% 一直不下来，LogPuller 是不是就一直 hang 住了？

> ⚠️ **注意**：当前运行时逻辑只评估 Area 级别的 pause/resume（参见 `memory_control.go:226`），Path 级别阈值（20%/10%）定义于算法接口但未被调用。

**解答**：

理论上确实存在风险，但实际上**不会永远 hang 住**，原因如下：

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                              Puller 算法为什么不会永远 hang                                   │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │  1. DynamicStream 会持续消费事件                                                       │   │
│   │                                                                                     │   │
│   │     LogPuller ──> DynamicStream ──> EventStore ──> EventService                      │   │
│   │                          │                                                          │   │
│   │                          │                                                          │   │
│   │                          ▼                                                          │   │
│   │                    handleLoop() 持续运行                                              │   │
│   │                    消费 pendingQueue                                                  │   │
│   │                    ↓                                                                 │   │
│   │                    pendingSize 下降                                                   │   │
│   │                    ↓                                                                 │   │
│   │                    memoryRatio 下降                                                   │   │
│   │                    ↓                                                                 │   │
│   │                    触发 ResumeArea                                                    │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │  2. 只有消费者完全卡死才会一直 hang                                                    │   │
│   │                                                                                     │   │
│   │     如果 handleLoop() 完全停止：                                                      │   │
│   │     - pendingQueue 不再消费                                                          │   │
│   │     - pendingSize 不再下降                                                           │   │
│   │     - memoryRatio 保持高位                                                           │   │
│   │     - 永远不会触发 ResumeArea                                                        │   │
│   │                                                                                     │   │
│   │     这种情况属于系统级故障，需要监控告警处理                                            │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │  3. TiKV 侧的缓冲能力（有条件）                                                        │   │
│   │                                                                                     │   │
│   │     即使 LogPuller hang 住：                                                         │   │
│   │     - TiKV 会在 GC safepoint 时间窗口内缓存未消费数据                                   │   │
│   │     - 前提：暂停时间 < tikv_gc_life_time（默认 10m）                                   │   │
│   │     - 超时风险：超过 GC safepoint 后数据会被清理                                        │   │
│   │     - 恢复后：在 safepoint 内可继续拉取                                                │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**总结**：Puller 算法的设计假设是"消费者正常工作"。如果消费者卡死，这是系统级故障，应该通过监控告警处理，而不是在内存控制层面解决。

---

#### Q2: EventCollector 直接 release 了，数据不会丢失吗？上游怎么知道扔掉了？

**问题**：EventCollector 算法直接清空队列丢弃事件，数据不会丢失吗？上游（EventService）怎么知道数据被扔掉了？

**解答**：

**数据不会永久丢失**，通过 **heartbeat + checkpointTs + EventStore 持久化** 机制保证：

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                              EventCollector 丢弃数据后的恢复机制                              │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                          关键机制 1：EventStore 持久化                                │   │
│   │                                                                                     │   │
│   │   EventStore 使用 PebbleDB 持久化存储所有从 TiKV 拉取的事件                            │   │
│   │   即使 EventCollector 丢弃了队列中的事件，数据仍在 EventStore 中                       │   │
│   │                                                                                     │   │
│   │   代码：logservice/eventstore/event_store.go                                         │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                    关键机制 2：checkpointTs + heartbeat + DropEvent reset            │   │
│   │                                                                                     │   │
│   │   • Dispatcher 维护 checkpointTs（已处理事件的时间戳进度）                              │   │
│   │   • checkpointTs 语义：通常是已写入下游的时间戳，但也有例外                              │   │
│   │     - 正常：已 flush 事件的 commitTs - 1                                             │   │
│   │     - 空队列时：max(checkpointTs, resolvedTs) 作为 fallback                           │   │
│   │     - 特殊场景：PassBlockEventToSink 直接 pass-through                                │   │
│   │   • Heartbeat 机制（见下方详细说明）                                                  │   │
│   │   • EventService 根据 checkpointTs 从 EventStore 扫描数据发送                          │   │
│   │                                                                                     │   │
│   │   代码：                                                                             │   │
│   │   • downstreamadapter/dispatcher/table_progress.go:174-185 (GetCheckpointTs)        │   │
│   │   • downstreamadapter/dispatcher/basic_dispatcher.go:485-494 (checkpointTs 逻辑)    │   │
│   │   • downstreamadapter/dispatchermanager/task.go:49-60 (heartbeat task)              │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                    关键机制 3：DropEvent 触发 Dispatcher reset                        │   │
│   │                                                                                     │   │
│   │   ReleasePath 丢弃事件时：                                                           │   │
│   │   1. OnDrop 创建 DropEvent (helper.go:169-174)                                      │   │
│   │   2. handleDropEvent 接收并调用 reset() (dispatcher_stat.go:604-610)                │   │
│   │   3. reset() 触发与 EventService 重新握手                                            │   │
│   │   4. EventService 从 checkpointTs 重新发送数据                                        │   │
│   │                                                                                     │   │
│   │   代码：                                                                             │   │
│   │   • downstreamadapter/eventcollector/helper.go:169-174 (OnDrop)                     │   │
│   │   • downstreamadapter/eventcollector/dispatcher_stat.go:604-610 (handleDropEvent)   │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   正常流程：                                                                                 │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                                                                                     │   │
│   │   EventStore ──(扫描)──> EventService ──(推送 Ts=100)──> EventCollector──>Dispatcher │   │
│   │                                │                                    │              │   │
│   │                                │                                    │              │   │
│   │                                │                              写入成功             │   │
│   │                                │                                    │              │   │
│   │                                │<──(heartbeat: checkpointTs=100)────┘              │   │
│   │                                │                                                   │   │
│   │                          下次扫描 Ts > 100                                           │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   ReleasePath 后恢复（实际流程）：                                                          │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                                                                                     │   │
│   │   1. ReleasePath 触发，队列中 Ts=100~200 的事件被丢弃                                 │   │
│   │                                                                                     │   │
│   │   2. OnDrop 创建 DropEvent，handleDropEvent 调用 reset()                             │   │
│   │      ┌────────────────────────────────────────────────────────────┐                 │   │
│   │      │ 关键：DropEvent 触发 dispatcher reset，而非仅依赖 heartbeat   │                 │   │
│   │      └────────────────────────────────────────────────────────────┘                 │   │
│   │                                                                                     │   │
│   │   3. reset() 触发与 EventService 重新握手                                            │   │
│   │                                                                                     │   │
│   │   4. EventService 从 checkpointTs（仍为 99）重新扫描 EventStore                       │   │
│   │      前提：EventStore 数据未被 GC，且 TiKV 数据在 GC safepoint 内                     │   │
│   │                                                                                     │   │
│   │   5. EventService 重新推送 Ts > 99 的事件                                             │   │
│   │                                                                                     │   │
│   │   6. 数据恢复，继续正常处理                                                          │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
│   Heartbeat 机制详解：                                                                       │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                                                                                     │   │
│   │   HeartBeatTask.Execute() (task.go:49-60):                                          │   │
│   │   • executeInterval = 200ms（task 执行间隔）                                         │   │
│   │   • completeStatusInterval = 50（即 10s / 200ms）                                    │   │
│   │   • needCompleteStatus = (statusTick % 50 == 0)                                      │   │
│   │                                                                                     │   │
│   │   实际行为：                                                                         │   │
│   │   • 每 200ms 执行一次 task loop                                                      │   │
│   │   • 完整状态上报（含所有 dispatcher checkpointTs）：约每 10s 一次                       │   │
│   │   • 简化上报（不含完整状态）：每 200ms                                                 │   │
│   │                                                                                     │   │
│   │   恢复触发方式：                                                                     │   │
│   │   • DropEvent -> reset() 是主动触发（即时）                                           │   │
│   │   • Heartbeat 是周期性同步（非即时）                                                  │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**总结**：

| 机制 | 作用 | 代码位置 |
|------|------|---------|
| **EventStore 持久化** | 数据副本，支持重新扫描 | `logservice/eventstore/event_store.go` |
| **checkpointTs** | 追踪已处理事件进度（有 fallback 机制） | `downstreamadapter/dispatcher/table_progress.go:174-185` |
| **DropEvent + reset** | 丢弃事件后主动触发重新握手 | `downstreamadapter/eventcollector/dispatcher_stat.go:604-610` |
| **heartbeat** | 周期性同步状态（完整状态约 10s/次） | `downstreamadapter/dispatchermanager/task.go:49-60` |

| 场景 | 结果 |
|------|------|
| 事件被丢弃 | OnDrop 创建 DropEvent -> reset() 重新握手 |
| Dispatcher 的 checkpointTs | 保持旧值，作为重新扫描起点 |
| 恢复机制 | DropEvent 触发 reset -> EventService 从 checkpointTs 重新推送 |
| 数据丢失？ | **不会永久丢失**（前提：EventStore/TiKV 数据未超 GC） |
| 代价 | 重新拉取增加网络/CPU 开销，延迟增加 |

---

#### Q3: 两种算法为什么不能互换？

**问题**：为什么 LogPuller 不能用 ReleasePath，EventCollector 不能用 Pause/Resume？

**解答**：

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                                    为什么不能互换算法                                         │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   场景 1：如果 LogPuller 用 ReleasePath（丢弃）                                              │
│   ─────────────────────────────────────────────────────                                      │
│                                                                                             │
│   TiKV ──(数据)──> LogPuller ──(丢弃)──> ??                                                 │
│   │                                                                                         │
│   │  ❌ 数据被丢弃后，无法从任何地方恢复                                                      │
│   │  ❌ LogPuller 是数据源头，没有"上游"可以重新拉取                                          │
│   │  ❌ 会造成永久性数据丢失                                                                 │
│   │                                                                                         │
│   └─────────────────────────────────────────────────────────────────────────────────────────│
│                                                                                             │
│   场景 2：如果 EventCollector 用 Pause/Resume（阻塞）                                        │
│   ───────────────────────────────────────────────────────────────────────────────────        │
│                                                                                             │
│   EventService ──(数据)──> EventCollector ──(阻塞)──> 等待...                                │
│   │                              │                                                          │
│   │                              │                                                          │
│   │  ⚠️ 如果下游（MySQL）写入慢，EventCollector 会持续阻塞                                   │
│   │  ⚠️ 内存持续增长，因为不丢弃数据                                                         │
│   │  ⚠️ 最终可能 OOM 崩溃                                                                   │
│   │                                                                                         │
│   │  但！如果使用 Pause/Resume：                                                            │
│   │  - EventService 会暂停发送                                                              │
│   │  - EventService 内存也会增长                                                            │
│   │  - 问题向上传导，可能导致整个系统崩溃                                                     │
│   │                                                                                         │
│   └─────────────────────────────────────────────────────────────────────────────────────────│
│                                                                                             │
│   正确的设计：                                                                               │
│   ┌─────────────────────────────────────────────────────────────────────────────────────┐   │
│   │                                                                                     │   │
│   │   LogPuller（源头）     →    用 Pause/Resume    →    阻塞不丢，TiKV 缓存              │   │
│   │                                                                                     │   │
│   │   EventCollector（中间） →   用 ReleasePath     →    丢弃可恢复，保护系统              │   │
│   │                                                                                     │   │
│   └─────────────────────────────────────────────────────────────────────────────────────┘   │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**总结**：算法选择是根据组件在数据流中的**位置**和**可恢复性**决定的，不是随意选择。

---

#### Q5: 如果一个 changefeed 的 area 超过 80%，所有表的 LogPuller 都会停下来吗？

**问题**：上游的 paused 标志是全局的，如果 changefeed A 的 area 超过 80%，会不会导致 changefeed B、C、D 的上游也都停下来？

**解答**：

**是的，这是当前实现的一个设计问题**：

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                              上游 paused 是全局的问题                                      │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   subscriptionClient (全局单例)                                                         │
│   └── paused atomic.Bool  // ⚠️ 只有一个全局的 paused 标志                        │
│       │                                                                             │
│       ▼                                                                             │
│   totalSpans.spanMap map[SubscriptionID]*subscribedSpan                            │
│       ├── SubscriptionID=1 → Changefeed A, Table A                                  │
│       ├── SubscriptionID=2 → Changefeed B, Table B                                  │
│       └── SubscriptionID=3 → Changefeed C, Table C                                  │
│                                                                                             │
│   当任何一个 area 超过 80%:                                                     │
│   └── s.paused.Store(true)  // ⚠️ 所有订阅全部暂停！                              │
│                                                                                             │
│   结果：Changefeed A 压力大 → Changefeed B、C 的上游也停止                          │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**补充说明 - paused 时如何处理数据**：
- **不会丢数据**：`pushRegionEventToDS` 会阻塞等待（`cond.Wait()`）
- **整个链路阻塞**：regionRequestWorker 会等待 pushRegionEventToDS 返回
- **TiKV 也会停止发送**：gRPC Stream 阻塞，不发送新数据
- **数据在 TiKV 缓存**：在 GC safepoint 内数据安全

**架构影响**：
- **LogPuller 是全局单例**：不是"有几张表就有几个 LogPuller"
- **所有表共享同一个 paused 标志**：任何一个 area 触发暂停会影响所有表

---

#### Q4: 两种算法的实际运行机制有什么区别？

**问题**：上游用 Puller 算法，下游用 EventCollector 算法，它们的实际运行机制有什么本质区别？

**解答**：

```
┌─────────────────────────────────────────────────────────────────────────────────────────────┐
│                              两种算法的实际运行机制对比                                  │
├─────────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                             │
│   上游 LogPuller + Puller 算法                                                          │
│   ────────────────────────────────────                                                     │
│   运行时：只调用 ShouldPauseArea                                                          │
│   • Area pause 阈值：80% 暂停，50% 恢复 ✅ 实际生效                                  │
│   • Path pause 阈值：20%/10% ❌ 未生效（定义了但未调用）                              │
│   • 机制：Pause/Resume（阻塞/恢复）                                                   │
│   • 渐进式控制：❌ 无（只有 Area 全或无）                                             │
│                                                                                             │
│   ────────────────────────────────────────────────────────────────────────────────────────   │
│                                                                                             │
│   下游 EventCollector + EventCollector 算法                                              │
│   ──────────────────────────────────────────────                                             │
│   运行时：不关心 Path/Area 的 pause/resume                                               │
│   • ShouldPauseArea：始终返回 false ❌ 不触发                                          │
│   • ShouldPausePath：定义了但未调用 ❌ 未生效                                         │
│   • 机制：ReleasePath（直接清空队列） ✅ 实际生效                                      │
│   • 控制粒度：Path 级别（逐个释放队列）                                              │
│                                                                                             │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

**关键区别总结：**

| 维度 | 上游 | 下游 EventCollector |
|------|---------------------|-------------------|
| **是否使用 pause/resume** | ✅ 是 | ❌ 否 |
| **控制级别** | Area（整个订阅） | Path（单个表） |
| **渐进式控制** | ❌ 无（只有全或无） | ✅ 有（逐个释放） |
| **核心机制** | Pause/Resume | ReleasePath |

**核心要点：**
1. **上游"没有渐进式"**：只有 Area 级别的 50/80 阈值，Path 级别的 20/10 虽然定义了但未生效
2. **下游"不用关心 path 和 area 的 pause"**：EventCollector 算法的 ShouldPauseArea 始终返回 false，核心就是**直接 release path**
3. **下游的"渐进式"体现在 ReleasePath**：不是逐个暂停，而是**逐个释放队列**，达到类似渐进式的效果

---

<a id="sec-c5-new-arch"></a>
### 5 新架构其他变化

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
    - 机制：Pause/Resume（阻塞/恢复）⚠️ **运行时只有 Area 级别生效**
    - Area pause 阈值：80% 暂停，50% 恢复 ✅ 实际生效
    - Path pause 阈值：20%/10% ❌ 定义但未调用（运行时未生效）
    - 特点：数据完整性保证，适合上游有缓冲能力的场景
    - 使用者：LogPuller
    - 参考：`utils/dynstream/memory_control_algorithm.go:43-76`。

- **EventCollector 算法**：`MemoryControlForEventCollector=1`，为中间处理层设计的算法。
    - 机制：ReleasePath（丢弃/清空）✅ **不使用 Pause/Resume**
    - ShouldPauseArea：始终返回 false ❌ 不触发暂停
    - 特点：快速释放内存，适合可从上游重新获取数据的场景
    - 使用者：EventCollector
    - 参考：`utils/dynstream/memory_control_algorithm.go:159-163`。

- **LogPuller**：上游组件，从 TiKV 拉取变更数据。
    - **拉取内容**：TiKV 的原始 KV 变更数据（Raw KV Changes）
    - 使用 Puller 算法 + Pause/Resume 机制
    - 内存配额：1GB（硬编码）
    - 参考：`logservice/logpuller/subscription_client.go`。

- **Subscription（订阅）**：LogPuller 中的订阅管理单位，对应一个表的完整数据订阅。
    - **关键特点**：一个订阅管理一个表的完整 span 范围内的所有 region
    - **组织结构**：subscriptionClient（全局单例）包含多个 subscriptionID
      ```
      subscriptionClient (全局单例)
      └── totalSpans.spanMap map[SubscriptionID]*subscribedSpan
          ├── SubscriptionID=1 → Table A 的完整 span (StartKey-A, EndKey-A)
          │                       └── 管理该范围内的 ALL regions (Region 1, 2, 3, ...)
          ├── SubscriptionID=2 → Table B 的完整 span
          └── SubscriptionID=3 → Table C 的完整 span
      ```
    - **重要说明**：**不是每个表一个独立的 LogPuller**，而是一个全局的 subscriptionClient 管理所有表
    - **与 Region 的关系**：
      - 一个订阅包含该表 span 范围内的所有 region
      - Region split 后，新 region 仍由原订阅管理
      - 订阅之间是独立的，不是"多个订阅合起来组成一个完整订阅"
    - **创建时机**：EventStore 为每个 dispatcher 创建一个订阅（`event_store.go:642`）
    - **映射关系**：SubscriptionID → Dispatcher → Table
    - 参考：`logservice/logpuller/subscription_client.go:109-129`、`logservice/logpuller/subscription_client.go:202-205`。

- **SubscribedSpan**：订阅的具体实现，存储订阅的 span 范围和状态。
    - 包含：subID（订阅ID）、span（表的 key 范围）、startTs（开始时间戳）、resolvedTs 等
    - 参考：`logservice/logpuller/subscription_client.go:109-129`。

- **订阅与 Dispatcher 的关系**：通常是一一对应关系（都是表级别），但存在特殊情况。
    - **通常情况（一一对应）**：
      ```
      Table A → Dispatcher A1 → SubscriptionID=1 (Table A 的完整 span)
      Table B → Dispatcher B1 → SubscriptionID=2 (Table B 的完整 span)
      ```
    - **特殊情况（一对多）**：当 Dispatcher 的 span 切换时，可能同时关联两个订阅：
      ```
      Table C → Dispatcher C1 → SubscriptionID=3 (原 span, subStat)
                                 → SubscriptionID=4 (新 span, pendingSubStat)
      ```
      注：参考 `event_store.go:118-125` 关于 subStat/pendingSubStat 的说明
    - **连接方式**：订阅和 Dispatcher 通过 EventStore 连接，不是直接关联
      ```
      EventStore.tableStats[TableID][SubscriptionID] = subscriptionStat
      ```
    - **相同点**：两者都是表级别的组织单位
    - **不同点**：
      - 订阅：LogPuller 层面，管理从 TiKV 拉取的数据
      - Dispatcher：EventCollector 层面，管理发送到下游的数据
    - 参考：`logservice/eventstore/event_store.go:589`。

- **EventCollector**：下游组件，作为 EventService 与 Dispatcher 之间的中继。
    - **写入下游**：Dispatcher → Sink 写入 MySQL/Kafka/其他存储
    - **注意：名字有误导性**，实际职责是"路由/分发"
    - 使用 EventCollector 算法 + ReleasePath 机制
    - 内存配额：changefeed 配置
    - 参考：`downstreamadapter/eventcollector/event_collector.go`。

- **Area**：DynamicStream 中的分组概念，用于内存统计。
    - EventCollector 中映射为 ChangefeedID
    - 参考：`utils/dynstream/interfaces.go:26`。

- **Path**：DynamicStream 中的目的端标识，对应一个事件队列。
    - EventCollector 中映射为 DispatcherID（**每个表一个 Dispatcher**）
    - 参考：`utils/dynstream/interfaces.go:23`。

- **MemoryQuota**：changefeed 内存配额（字节），默认 1GB。
    - 参考：`pkg/config/server.go:45`、`pkg/config/replica_config.go:47`。

- **ReleasePath**：dynstream 反馈类型，表示释放/丢弃某 path 的队列事件。
    - 参考：`utils/dynstream/interfaces.go:281-289`。

- **PauseArea/ResumeArea**：dynstream 反馈类型，用于 Puller 算法。
    - PauseArea：暂停该 area 的所有事件入队
    - ResumeArea：恢复该 area 的事件入队
    - 参考：`utils/dynstream/interfaces.go`。
