# Memory Controller 报告

说明：以下按“入口 -> 逻辑 -> 最底层释放”的链路组织；每条记录包含 `文件:行号`、代码片段、说明。

目录:
- [A. Memory Controller 运行机制详解（入口 -> 逻辑 -> 释放）](#a-memory-controller)
 - [1 配额来源与可配置入口](#sec-1-memory-quota)
 - [2 新架构入口：EventCollector 启用 memory control](#sec-2-enable-memory-control)
 - [3 changefeed 配额绑定到 AreaSettings（changefeed -> dispatcher -> dynstream）](#sec-3-quota-area)
 - [4 dynstream 把 path 加入 area 并挂上 memControl](#sec-4-add-path-area)
 - [5 内存统计与控制核心（append/ratio/释放）](#sec-5-core-control)
 - [6 ReleasePath 反馈执行链（从入口到最底层）](#sec-6-releasepath-flow)
 - [7 Pause/Resume 逻辑现状（新架构 vs 老架构）](#sec-7-pause-resume)
- [B. 参考章节：新架构数据流上层逻辑](#b-highlevel-flow)
 - [8 新架构数据流上层逻辑](#sec-8-highlevel)
- [C. 术语汇总小节](#c-terminology)

<a id="a-memory-controller"></a>
## A. Memory Controller 运行机制详解（入口 -> 逻辑 -> 释放）

summary：
- **文档范围**
    - 覆盖新架构 [EventCollector](#c-terminology) + [DynamicStream](#c-terminology) 的 [memory controller](#c-terminology) 链路（包含 [path](#c-terminology)/[area](#c-terminology) 统计与 [ReleasePath](#c-terminology)）。
- **不涉及内容**
    - EventService scan 限流与下游 sink 写入行为（见第 8 节的上层链路参考）。
- **关键组件**
    - [EventCollector](#c-terminology)：内存控制入口与反馈汇聚。
    - [memory controller](#c-terminology)：执行统计、阈值判断与释放策略。
    - [path](#c-terminology)/[area](#c-terminology)：内存统计的最小粒度与分组边界。
- **主数据流**
    - changefeed 配额 -> EventCollector.AddDispatcher -> [DynamicStream](#c-terminology) [area](#c-terminology)/[path](#c-terminology) -> appendEvent -> releaseMemory -> [ReleasePath](#c-terminology) 反馈 -> 清空 [path](#c-terminology) 队列。
- **关键策略**
    - deadlock 与高水位两类触发入口；阈值与释放比例见第 5 节。
    - [EventCollector](#c-terminology) 算法不走 pause/resume（见第 7 节）。
- **降级机制**
    - [ReleasePath](#c-terminology) 通过"丢弃/清空"降内存；可丢弃事件走 OnDrop 分支（见第 5/6 节）。
- **可观测性**
    - 以内存占用比例与 pendingSize 为核心判定依据（见第 5 节的 memoryUsageRatio/totalPendingSize）。
- **前置条件**
    - 新架构开关 [newarch](#c-terminology) 启用且 [EventCollector](#c-terminology) 正常运行（见第 2 节）。

架构层次图：
```
╔═════════════════════════════════════════════════════════════════════════════════════════════════╗
║                                        业务层 (Business Layer)                                   ║
╠═════════════════════════════════════════════════════════════════════════════════════════════════╣
║                                                                                                 ║
║   Changefeed (复制任务)                                                                          ║
║   ├── 配置：MemoryQuota, SyncPoint, Redo...                                                     ║
║   ├── 包含多个 Dispatcher                                                                        ║
║   │                                                                                             ║
║   │   ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                                        ║
║   │   │ Dispatcher  │  │ Dispatcher  │  │ Dispatcher  │  ← 每个表一个 Dispatcher                ║
║   │   │ (table_1)   │  │ (table_2)   │  │ (table_N)   │                                        ║
║   │   │             │  │             │  │             │                                        ║
║   │   │ - 处理事件   │  │ - 处理事件   │  │ - 处理事件   │                                        ║
║   │   │ - 写入下游   │  │ - 写入下游   │  │ - 写入下游   │                                        ║
║   │   └─────────────┘  └─────────────┘  └─────────────┘                                        ║
║   │                                                                                             ║
║   └── 关系：Changefeed 1 : N Dispatcher (包含关系)                                               ║
║                                                                                                 ║
╚═════════════════════════════════════════════════════════════════════════════════════════════════╝
                                         │
                                         │ EventCollector 把业务概念映射到 Dynstream 抽象
                                         │   Changefeed → Area
                                         │   Dispatcher → Path
                                         ▼
╔═════════════════════════════════════════════════════════════════════════════════════════════════╗
║                                    基础设施层 (Infrastructure Layer)                              ║
╠═════════════════════════════════════════════════════════════════════════════════════════════════╣
║                                                                                                 ║
║   DynamicStream (通用事件处理框架，package: dynstream)                                            ║
║   ├── 职责：事件分发、队列管理、内存控制                                                            ║
║   ├── 本身不处理业务逻辑，只提供框架能力                                                            ║
║   │                                                                                             ║
║   │   ┌─────────────────────────────────────────────────────────────────────────────────────┐   ║
║   │   │  memControl (内存控制器)                                                              │   ║
║   │   │    └── areaMap: map[Area]*areaMemStat                                                │   ║
║   │   │                                                                                     │   ║
║   │   │          Area (对应 Changefeed)                                                      │   ║
║   │   │          ├── maxPendingSize = MemoryQuota                                            │   ║
║   │   │          ├── totalPendingSize (所有 path 的内存总和)                                   │   ║
║   │   │          ├── algorithm (EventCollector/Puller)                                       │   ║
║   │   │          │                                                                           │   ║
║   │   │          │   ┌─────────┐  ┌─────────┐  ┌─────────┐                                  │   ║
║   │   │          │   │ Path    │  │ Path    │  │ Path    │  ← 对应 Dispatcher                │   ║
║   │   │          │   │         │  │         │  │         │                                  │   ║
║   │   │          │   │ pending │  │ pending │  │ pending │                                  │   ║
║   │   │          │   │ Queue   │  │ Queue   │  │ Queue   │                                  │   ║
║   │   │          │   └─────────┘  └─────────┘  └─────────┘                                  │   ║
║   │   │          │                                                                           │   ║
║   │   │          └── 关系：Area 1 : N Path (分组关系，用于内存统计)                             │   ║
║   │   │                                                                                     │   ║
║   │   └─────────────────────────────────────────────────────────────────────────────────────┘   ║
║   │                                                                                             ║
║   │   ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                                    ║
║   │   │  stream[0]   │  │  stream[1]   │  │  stream[N]   │  ← 并行处理 worker                  ║
║   │   └──────────────┘  └──────────────┘  └──────────────┘                                    ║
║   │                                                                                             ║
║   └── 注：DynamicStream 是通用框架，可用于其他场景（如 LogPuller 也用它）                           ║
║                                                                                                 ║
╚═════════════════════════════════════════════════════════════════════════════════════════════════╝

关键区别：
┌─────────────────┬──────────────────────────────────────────────────────────────────┐
│ 层次            │ 说明                                                             │
├─────────────────┼──────────────────────────────────────────────────────────────────┤
│ 业务层          │ Changefeed/Dispatcher 是 TiCDC 的业务概念，有实际的包含关系        │
│ 基础设施层      │ Area/Path 是 Dynstream 的抽象概念，用于事件分组和内存统计          │
│ 映射关系        │ EventCollector 负责把业务概念映射到基础设施抽象                    │
└─────────────────┴──────────────────────────────────────────────────────────────────┘
```

时序图（完整流程）：
```
                                    ╔═════════════════════════════════════════════════════════════════════════════════╗
                                    ║                        Phase 1: 初始化与配额绑定                              ║
                                    ╚═════════════════════════════════════════════════════════════════════════════════╝

DispatcherManager               EventCollector                DynamicStream                 memControl                areaMemStat
      |                              |                             |                            |                           |
      | --(MemoryQuota)------------> |                             |                            |                           |
      |                              |                             |                            |                           |
      |                              | --(AddDispatcher)---------> |                            |                           |
      |                              |   [dispatcher, quota]       |                            |                           |
      |                              |                             |                            |                           |
      |                              |                             | --(AddPath)--------------> |                           |
      |                              |                             |   [path, AreaSettings]     |                           |
      |                              |                             |                            |                           |
      |                              |                             |                            | --(addPathToArea)-------> |
      |                              |                             |                            |   [创建/复用 area]        |
      |                              |                             |                            |                           |
      |                              |                             |                            | <---(areaMemStat)-------- |
      |                              |                             |                            |   [绑定 path.areaMemStat] |
      |                              |                             |                            |                           |
      |                              |                             | <----(path 绑定完成)------- |                           |
      |                              |                             |                            |                           |

                                    ╔═════════════════════════════════════════════════════════════════════════════════╗
                                    ║                        Phase 2: 事件入队与内存检测                           ║
                                    ╚═════════════════════════════════════════════════════════════════════════════════╝

EventService                    DynamicStream                  areaMemStat                 releaseMemory              feedbackChan
      |                              |                             |                            |                           |
      | --(send events)------------> |                             |                            |                           |
      |                              |                             |                            |                           |
      |                              | --(appendEvent)-----------> |                            |                           |
      |                              |   [event, handler]          |                            |                           |
      |                              |                             |                            |                           |
      |                              |                             | [checkDeadlock]            |                           |
      |                              |                             |   有入队 && 无出队?         |                           |
      |                              |                             |   memoryUsageRatio > 60%?  |                           |
      |                              |                             |                            |                           |
      |                              |                             | ===[deadlock 检测通过]====> |                           |
      |                              |                             |                            |                           |
      |                              |                             |                            | [选择 blocking path]      |
      |                              |                             |                            | [按 lastHandleEventTs     |
      |                              |                             |                            |  降序排序]                 |
      |                              |                             |                            |                           |
      |                              |                             |                            | --(ReleasePath)---------> |
      |                              |                             |                            |   [path, FeedbackType]    |
      |                              |                             |                            |                           |
      |                              |                             |                            | [droppable?]              |
      |                              |                             |                            |   handler.OnDrop()        |
      |                              |                             |                            |                           |
      |                              |                             | <----(release 完成)-------- |                           |
      |                              |                             |                            |                           |
      |                              |                             | [pendingQueue.PushBack]    |                           |
      |                              |                             | [updatePendingSize]        |                           |
      |                              |                             | [totalPendingSize.Add]     |                           |
      |                              |                             |                            |                           |

                                    ╔═════════════════════════════════════════════════════════════════════════════════╗
                                    ║                        Phase 3: ReleasePath 反馈清空                         ║
                                    ╚═════════════════════════════════════════════════════════════════════════════════╝

feedbackChan                    EventCollector                 DynamicStream                  stream                   eventQueue
      |                              |                             |                            |                           |
      | --(ReleasePath)------------> |                             |                            |                           |
      |                              |                             |                            |                           |
      |                              | [processDSFeedback]         |                            |                           |
      |                              |                             |                            |                           |
      |                              | --(ds.Release(path))------> |                            |                           |
      |                              |                             |                            |                           |
      |                              |                             | --(addEvent)-------------> |                           |
      |                              |                             |   [release=true, pathInfo] |                           |
      |                              |                             |                            |                           |
      |                              |                             |                            | [handleLoop]              |
      |                              |                             |                            |                           |
      |                              |                             |                            | --(releasePath)--------> |
      |                              |                             |                            |   [pathInfo]              |
      |                              |                             |                            |                           |
      |                              |                             |                            |                           | [PopFront 逐个丢弃]
      |                              |                             |                            |                           | [decPendingSize]
      |                              |                             |                            |                           | [totalPendingSize.Add(-size)]
      |                              |                             |                            |                           | [pendingSize.Store(0)]
      |                              |                             |                            |                           |
      |                              |                             |                            | <----(完成)-------------- |
      |                              |                             | <----(完成)--------------- |                           |
      |                              | <----(完成)---------------- |                            |                           |
      |                              |                             |                            |                           |

关键阈值说明：
┌─────────────────────┬───────────────────────────────────────────────────────────────────┐
│ 阈值名称            │ 说明                                                              │
├─────────────────────┼───────────────────────────────────────────────────────────────────┤
│ deadlock 窗口       │ 5s 内有入队 && 5s 内无出队                                        │
│ deadlock 高水位     │ memoryUsageRatio > 60% (1 - defaultReleaseMemoryRatio)           │
│ 高水位强制释放      │ memoryUsageRatio >= 150%                                          │
│ 释放比例            │ totalPendingSize * 40% (defaultReleaseMemoryRatio)               │
│ 释放最小阈值        │ pendingSize >= 256 (defaultReleaseMemoryThreshold)               │
└─────────────────────┴───────────────────────────────────────────────────────────────────┘
```

<a id="sec-1-memory-quota"></a>
### 1 配额来源与可配置入口

summary：说明 [MemoryQuota](#c-terminology) 的来源（默认值、配置入口）与 dynstream 兜底默认值。要点如下：
- MemoryQuota 在 cdc 启动阶段就作为配置项引入/校验。
- 真正生效（作为 [area](#c-terminology) 的上限）是在 changefeed 注册到 [EventCollector](#c-terminology) 时（见第 3 节）。
- dynstream 对未设置的 area 也有 1GB 的默认兜底。

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

<a id="sec-2-enable-memory-control"></a>
### 2 新架构入口：EventCollector 启用 memory control

summary：说明“是否必然启用 [memory controller](#c-terminology)”的判断链路。要点如下：
- [newarch](#c-terminology)=true 时进入新架构 server。
- 新架构会启动 [EventCollector](#c-terminology)。
- EventCollector 动态流硬编码启用 EnableMemoryControl。

调用链：
- 新架构开关（[newarch](#c-terminology)）
 - newarch=true 时进入新架构 server
  - setPreServices 创建 [EventCollector](#c-terminology)
   - [EventCollector](#c-terminology) 动态流启用 EnableMemoryControl

重要结论：**新架构必然启用 [memory controller](#c-terminology)**（EventCollector 动态流硬编码 `EnableMemoryControl=true`）。

#### 2.1 新架构开关与入口（是否必然启用 EventCollector）
```golang
// pkg/config/server.go:91
Newarch:    false // newarch 默认关闭
// cmd/cdc/server/server.go:67
cmd.Flags().BoolVarP(&o.serverConfig.Newarch, "newarch", "x", o.serverConfig.Newarch, "Run the new architecture of TiCDC server") // CLI 开关
// cmd/cdc/server/server.go:301
newarch = os.Getenv("TICDC_NEWARCH") == "true" // 环境变量开关
// cmd/cdc/server/server.go:281
newarch = isNewArchEnabledByConfig(serverConfigFilePath) // 读取 server 配置文件的 newarch
// cmd/cdc/server/server.go:368
if isNewArchEnabled(o) { // newarch=true -> 新架构
// cmd/cdc/server/server.go:378
err = o.run(cmd) // 进入新架构 server 运行流程
// cmd/cdc/server/server.go:382
return runTiFlowServer(o, cmd) // newarch=false -> 旧架构流程
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

#### 2.3 memory control 算法选择（配置项/硬编码）

summary：说明 [memory controller](#c-terminology) 算法的选择与配置。要点如下：
- 算法类型只有两种：[MemoryControlForPuller](#c-terminology) 与 [MemoryControlForEventCollector](#c-terminology)。
- 当前没有用户可配置项；EventCollector 在创建 AreaSettings 时硬编码为 MemoryControlForEventCollector。
- 默认情况：新架构 [EventCollector](#c-terminology) 使用 MemoryControlForEventCollector；NewMemoryControlAlgorithm 在未指定为 EventCollector 算法时默认走 [Puller](#c-terminology) 算法。

```golang
// utils/dynstream/memory_control.go:28-34
MemoryControlForPuller = 0 // Puller 算法常量
MemoryControlForEventCollector = 1 // EventCollector 算法常量
// utils/dynstream/interfaces.go:274
algorithm: memoryControlAlgorithm // AreaSettings 保存算法类型
// downstreamadapter/eventcollector/event_collector.go:270
areaSetting := dynstream.NewAreaSettingsWithMaxPendingSize(memoryQuota, dynstream.MemoryControlForEventCollector, "eventCollector") // EventCollector 硬编码算法
// utils/dynstream/memory_control_algorithm.go:30-35
switch algorithm {
case MemoryControlForEventCollector:
  return &EventCollectorMemoryControl{}
default:
  return &PullerMemoryControl{} // 未指定时默认 Puller
}
```

---

<a id="sec-3-quota-area"></a>
### 3 changefeed 配额绑定到 AreaSettings（changefeed -> dispatcher -> dynstream）

summary：说明在注册 changefeed/dispatcher 时，MemoryQuota 被传入 dynstream，成为 area 的上限。要点如下：
- DispatcherManager 从 changefeed 配置读 MemoryQuota。
- EventCollector.AddDispatcher 把配额传给 dynstream。
- [area](#c-terminology) 的 `maxPendingSize` 直接等于 [MemoryQuota](#c-terminology)，[path](#c-terminology) 上限派生为 10%（最少 1MB）。

调用链：
- changefeed 配置
 - DispatcherManager.sinkQuota
  - EventCollector.AddDispatcher（[EventCollector](#c-terminology)）
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

<a id="sec-4-add-path-area"></a>
### 4 dynstream 把 path 加入 area 并挂上 memControl

summary：说明 dynstream 内部如何把 [path](#c-terminology) 归入 [area](#c-terminology)，并绑定 memControl。要点如下：
- AddPath 触发 setMemControl。
- memControl.addPathToArea 创建或复用 area 统计结构。
- path 绑定 areaMemStat，记录 path 数量并保存 settings。

时序图：
```
EventCollector                    DynamicStream                     memControl                    areaMemStat
    |                                  |                                |                              |
    | --(AddDispatcher)-------------> |                                |                              |
    |                                  |                              |                              |
    |                                  | --(AddPath)-----------------> |                              |
    |                                  |   [path, AreaSettings]        |                              |
    |                                  |                                |                              |
    |                                  |                    [setMemControl]                             |
    |                                  |                                |                              |
    |                                  |                                | --(addPathToArea)----------> |
    |                                  |                                |   [path, settings, feedback] |
    |                                  |                                |                              |
    |                                  |                                |               [查找或创建 area]|
    |                                  |                                |                              |
    |                                  |                                |      [绑定 path.areaMemStat]  |
    |                                  |                                |                              |
    |                                  |                                | <----(areaMemStat)---------- |
    |                                  |                                |                              |
    |                                  | <-----(path 绑定完成)----------|                              |
    |                                  |                                |                              |
    | <----(AddDispatcher 完成)------- |                                |                              |
    |                                  |                                |                              |

注：areaMemStat 负责统计 area 级 pendingSize，path 持有其引用以更新统计。
```

调用链：
- DynamicStream.AddPath
 - setMemControl
  - memControl.addPathToArea
   - newAreaMemStat / 绑定 settings（[area](#c-terminology)/[path](#c-terminology)）

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

<a id="sec-5-core-control"></a>
### 5 内存统计与控制核心（append/ratio/释放）

summary：说明事件入队时的内存统计、阈值判定、死锁检测与释放策略（核心控制逻辑）。结构化说明如下：
- **入队前处理（入队到 path 队列前）**
    - 对 [PeriodicSignal](#c-terminology) 做"最后一条覆盖"合并，避免信号膨胀。
- **releaseMemory 的触发入口（仅 EventCollector 算法）**
    - 死锁检测分支：满足"5s 内有事件进入 path 队列且 5s 内无 size 减少"并且"内存占用 > 60%"时触发 releaseMemory。
    - 高水位分支：内存占用比例 >= 1.5（150%）时立即触发 releaseMemory，并对可丢弃事件（[Droppable](#c-terminology)）调用 OnDrop 转换为 drop 事件并入队到 path 队列。
- **releaseMemory 的执行规则**
    - 按 lastHandleEventTs 降序挑选 [path](#c-terminology)，只释放 blocking 且 pendingSize >= 256 的 path。
    - 目标释放量为总 pending 的 40%，通过 [ReleasePath](#c-terminology) 反馈通知下游执行清理。
- **统计更新**
    - 最终将事件入队到 path 队列并更新 [path](#c-terminology)/[area](#c-terminology) 的 pendingSize 统计。

时序图：
```
path.appendEvent                areaMemStat                    releaseMemory               feedbackChan
      |                             |                              |                           |
      | --(appendEvent)-----------> |                              |                           |
      |   [event, handler]          |                              |                           |
      |                             |                              |                           |
      |                             | [checkDeadlock]              |                           |
      |                             |   hasEventComeButNotOut?     |                           |
      |                             |   memoryUsageRatio > 60%?    |                           |
      |                             |                              |                           |
      |                             |---[deadlock?]--------------->|                           |
      |                             |                              |                           |
      |                             |                              | [按 lastHandleEventTs     |
      |                             |                              |  降序选择 blocking path]  |
      |                             |                              |                           |
      |                             |                              | [发送 ReleasePath] ----> |
      |                             |                              |                           |
      |                             |---[high watermark?]--------->|                           |
      |                             |   memoryUsageRatio >= 150%?  |                           |
      |                             |                              |                           |
      |                             |                              | [droppable event?]        |
      |                             |                              |   handler.OnDrop()        |
      |                             |                              |                           |
      |                             | <-----(release 完成)--------- |                           |
      |                             |                              |                           |
      |                             | [pendingQueue.PushBack]      |                           |
      |                             | [updatePendingSize]          |                           |
      |                             | [totalPendingSize.Add]       |                           |
      |                             |                              |                           |
      | <-----(append 完成)-------- |                              |                           |
      |                             |                              |                           |

注：deadlock 条件 = (5s 内有入队 && 5s 内无出队) && (memoryUsageRatio > 60%)
注：high watermark 条件 = memoryUsageRatio >= 1.5 (150%)
注：releaseMemory 目标释放量 = totalPendingSize * 40%
```

术语说明：可丢弃事件（[Droppable](#c-terminology)）
- 含义：EventType.Droppable=true 的事件可被内存控制丢弃。
- 代码引用：见下方代码片段（EventType.Droppable 定义 + OnDrop 分支）。

术语说明：[PeriodicSignal](#c-terminology)
- 含义：一种“周期性信号”事件类型（如 resolvedTs），不携带业务数据，可用最新信号覆盖旧信号以减小队列压力。
- 代码引用：见下方代码片段（Property.PeriodicSignal 定义与注释）。

调用链：
- path.appendEvent（[path](#c-terminology)）
 - areaMemStat.appendEvent
  - 统计 size / 判定 deadlock / 触发 release

```golang
// utils/dynstream/stream.go:370
func (pi *pathInfo...) appendEvent(...) bool // path 收到事件，准备入队到 path 队列的入口
// utils/dynstream/stream.go:372
return pi.areaMemStat.appendEvent(pi, event, handler) // 事件交给 areaMemStat 做统一计量并最终入队到 path 队列
// utils/dynstream/memory_control.go:101
defer as.updateAreaPauseState(path) // 事件追加后更新 area 状态（新架构不触发暂停）
// utils/dynstream/memory_control.go:121
if as.checkDeadlock() { as.releaseMemory() } // 检测疑似死锁并触发释放
// utils/dynstream/memory_control.go:125
if as.memoryUsageRatio() >= 1.5 && ... // 内存>150% 且为 EventCollector 算法时强制释放
// utils/dynstream/memory_control.go:128
if event.eventType.Droppable { ... handler.OnDrop(...) } // 可丢弃事件会转换成 drop 事件并入队到 path 队列
// utils/dynstream/memory_control.go:152
path.pendingQueue.PushBack(event) // 事件入队到 path 队列
// utils/dynstream/memory_control.go:154
path.updatePendingSize(int64(event.eventSize)) // 更新 path 级待处理字节数
// utils/dynstream/memory_control.go:155
as.totalPendingSize.Add(int64(event.eventSize)) // 更新 area 级待处理字节总量
// utils/dynstream/memory_control.go:221
return float64(as.totalPendingSize.Load()) / float64(as.settings.Load().maxPendingSize) // 计算 area 内存占用比例
// utils/dynstream/memory_control.go:36
defaultReleaseMemoryRatio   = 0.4 // 释放比例默认 40%
// utils/dynstream/memory_control.go:37
defaultDeadlockDuration    = 5 * time.Second // 死锁判定窗口为 5 秒
// utils/dynstream/memory_control.go:38
defaultReleaseMemoryThreshold = 256 // 只释放 pendingSize>=256 的阻塞 path
// utils/dynstream/memory_control.go:167
hasEventComeButNotOut := ... // 死锁判定：有输入但无输出
// utils/dynstream/memory_control.go:169
memoryHighWaterMark := as.memoryUsageRatio() > (1 - defaultReleaseMemoryRatio) // 高水位阈值=60%
// utils/dynstream/memory_control.go:191
sizeToRelease := int64(float64(as.totalPendingSize.Load()) * defaultReleaseMemoryRatio) // 计算需要释放的总量
// utils/dynstream/memory_control.go:198
if ... !path.blocking.Load() { continue } // 只选择阻塞 path 进行释放
// utils/dynstream/memory_control.go:211
FeedbackType: ReleasePath // 发送 ReleasePath 反馈
```

可丢弃事件（[Droppable](#c-terminology)）相关代码片段：
```golang
// utils/dynstream/interfaces.go:41-48
type EventType struct { // EventType 内标记是否可丢弃
  DataGroup int
  Property Property
  Droppable bool
}
// utils/dynstream/memory_control.go:128
if event.eventType.Droppable { ... handler.OnDrop(...) } // 可丢弃事件触发 OnDrop
```

[PeriodicSignal](#c-terminology)相关代码片段：
```golang
// utils/dynstream/interfaces.go:59-69
// PeriodicSignal - Periodic signal events
// 1. Contains no actual data, only indicates occurrence of an event
// 2. System drops early duplicate signals to reduce load
// 3. Must continue sending even when path is paused (for memory control)
// 4. Should be small and consistent in size
// Example: resolvedTs
PeriodicSignal
```

---

<a id="sec-6-releasepath-flow"></a>
### 6 ReleasePath 反馈执行链（从入口到最底层）

summary：说明 [ReleasePath](#c-terminology) 反馈从 EventCollector 下发到 dynstream 清空队列的完整执行链路。关键步骤如下：
- EventCollector 的 processDSFeedback 仅处理 ReleasePath（分别来自 ds 与 redoDs）。
- 收到 ReleasePath 后调用 ds.Release(path)。
- dynstream 将 release 信号注入对应 stream。
- handleLoop 识别 release 事件并调用 eventQueue.releasePath 清空该 [path](#c-terminology) 队列。
- 清空后同步扣减 [area](#c-terminology)/[path](#c-terminology) 的 pendingSize，最终归零。

时序图：
```
memControl                     EventCollector                  DynamicStream                    stream                      eventQueue
    |                               |                               |                              |                              |
    | --(ReleasePath feedback)----> |                               |                              |                              |
    |   [path, FeedbackType]        |                               |                              |                              |
    |                               |                               |                              |                              |
    |                               | [processDSFeedback]           |                              |                              |
    |                               |   feedbackType == ReleasePath?|                              |                              |
    |                               |                               |                              |                              |
    |                               | --(ds.Release(path))--------> |                              |                              |
    |                               |                               |                              |                              |
    |                               |                               | --(addEvent)---------------->|                              |
    |                               |                               |   [release=true, pathInfo]   |                              |
    |                               |                               |                              |                              |
    |                               |                               |                              | [handleLoop 收到 release]    |
    |                               |                               |                              |                              |
    |                               |                               |                              | --(releasePath)------------> |
    |                               |                               |                              |   [pathInfo]                 |
    |                               |                               |                              |                              |
    |                               |                               |                              |                              | [pendingQueue.PopFront]
    |                               |                               |                              |                              | [逐个丢弃事件]
    |                               |                               |                              |                              |
    |                               |                               |                              |                              | [decPendingSize]
    |                               |                               |                              |                              | [areaMemStat.totalPendingSize.Add(-size)]
    |                               |                               |                              |                              | [path.pendingSize.Store(0)]
    |                               |                               |                              |                              |
    |                               |                               |                              | <----(release 完成)--------- |
    |                               |                               |                              |                              |
    |                               |                               | <----(Release 完成)---------- |                              |
    |                               |                               |                              |                              |
    |                               | <----(Release 完成)----------- |                              |                              |
    |                               |                               |                              |                              |

注：ReleasePath 是"丢弃/清空"操作，事件不会恢复，直接从 pendingQueue 移除。
注：areaMemStat.totalPendingSize 同步扣减，确保内存统计准确。
```

调用链：
- [EventCollector](#c-terminology) 接收 [ReleasePath](#c-terminology)
 - dynstream.Release
  - stream 收到 release 信号
   - eventQueue 清空 [path](#c-terminology)

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

<a id="sec-7-pause-resume"></a>
### 7 Pause/Resume 逻辑现状（新架构 vs 老架构）

summary：对比新架构（[EventCollector](#c-terminology) 算法）与旧架构（[Puller](#c-terminology) 算法）的 pause/resume 行为与阈值差异。要点如下：
- EventCollector 算法不触发 area pause/resume（仅计算比例）。
- Puller 算法有固定阈值：path 20/10，area 80/50。

#### 7.1 Pause/Resume 与 ReleasePath 对比

summary：
- Puller 算法通过 pause/resume 抑制 path/area 入队
- EventCollector 算法不 pause/resume，而是通过 ReleasePath 清理 pending 队列
- ReleasePath 是“丢弃/清空”，不是“暂停”

| 维度 | Puller 算法（Pause/Resume） | EventCollector 算法（ReleasePath） |
|------|----------------------------|------------------------------------|
| 触发条件 | path/area 达到固定比例阈值 | deadlock 或高水位触发 releaseMemory |
| 动作 | pause/resume 反馈，阻止/恢复入队 | ReleasePath 反馈，清空 path 待处理事件 |
| 结果 | pending 不再增长，但历史事件保留 | pending 直接下降，事件被丢弃 |
| 适用算法 | MemoryControlForPuller | MemoryControlForEventCollector |

说明：ReleasePath 的执行链路见第 6 节（ReleasePath 反馈执行链）。

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

<a id="b-highlevel-flow"></a>
## B. 参考章节：新架构数据流上层逻辑

<a id="sec-8-highlevel"></a>
### 8 新架构数据流上层逻辑

summary：给出上层数据流（[Puller](#c-terminology) / [Sinker](#c-terminology)）组件分工的代码验证参考。要点如下：
- Puller 侧：SubscriptionClient / EventStore / EventService。
- Sinker 侧：[EventCollector](#c-terminology)/ Dispatcher / Sink。
- 编排模块（如 HeartbeatCollector、DispatcherOrchestrator）不完全属于拉或写。

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
eventStore eventstore.EventStore // EventService 内部持有 EventStore，作为事件来源
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

<a id="c-terminology"></a>
## C. 术语汇总小节

- memory controller：dynstream 的内存控制模块，负责统计 pendingSize、触发 ReleasePath 等反馈。参考：`utils/dynstream/memory_control.go:293`、`utils/dynstream/memory_control.go:302`、`utils/dynstream/parallel_dynamic_stream.go:72-75`。
- MemoryQuota：changefeed 内存配额（字节）。默认 1GB；用于设置 area 的 maxPendingSize。参考：`pkg/config/server.go:45`、`pkg/config/replica_config.go:47`、`downstreamadapter/eventcollector/event_collector.go:270`。
- newarch：新架构开关。支持 `--newarch/-x`、`TICDC_NEWARCH=true`、配置 `newarch=true`。参考：`cmd/cdc/server/server.go:67`、`cmd/cdc/server/server.go:301`、`cmd/cdc/server/server.go:281`。
- EventCollector：下游侧入口网关组件，作为 EventService 与 Dispatcher 之间的中继。**注意：名字有误导性**，实际职责不是"收集"，而是"路由/分发"：
    - 接收 EventService 发来的事件（通过 MessageCenter）。
    - 通过 DynamicStream 把事件分发到对应的 Dispatcher。
    - 管理 Dispatcher 注册/注销（AddDispatcher / RemoveDispatcher）。
    - 处理内存控制反馈（ReleasePath），汇报可用内存给 EventService。
    - 更准确的名字应该是 EventRouter 或 EventRelay。
    - 参考：`downstreamadapter/eventcollector/event_collector.go:105-110`、`server/server.go:259`。
- DynamicStream（又名 dynstream）：通用事件处理框架，提供事件分发、队列管理、内存控制能力。
    - **本身不处理业务逻辑**，只提供基础设施能力。
    - 是通用框架，可用于多种场景（EventCollector、LogPuller 都用它）。
    - 包名是 `dynstream`，接口名是 `DynamicStream`，指同一个东西。
    - 参考：`utils/dynstream/parallel_dynamic_stream.go:30-46`。
- Area：DynamicStream 中的**分组概念**（类型参数），用于内存统计。
    - 在 EventCollector 中映射为 `ChangefeedID`。
    - 一个 Area 包含多个 Path，共享内存配额。
    - 参考：`utils/dynstream/interfaces.go:26`、`downstreamadapter/eventcollector/helper.go:157-159`。
- Path：DynamicStream 中的**目的端标识**（类型参数），对应一个事件队列。
    - 在 EventCollector 中映射为 `DispatcherID`。
    - 每个 Path 有独立的 pendingQueue 和 pendingSize。
    - 参考：`utils/dynstream/interfaces.go:23`、`downstreamadapter/eventcollector/helper.go:67-69`。
- EventCollector 算法：`MemoryControlForEventCollector=1`，EventCollector 动态流使用的内存控制算法。不走 pause/resume，而是用 ReleasePath 清空队列。参考：`utils/dynstream/memory_control.go:31`、`utils/dynstream/memory_control_algorithm.go:159-163`。
- Puller 算法：`MemoryControlForPuller=0`，`NewMemoryControlAlgorithm` 默认分支。使用 pause/resume 机制控制内存。参考：`utils/dynstream/memory_control.go:28-30`、`utils/dynstream/memory_control_algorithm.go:43-76`。
- PeriodicSignal：`EventType.Property` 的一种，表示周期性信号事件。参考：`utils/dynstream/interfaces.go:63-69`。
- Droppable：`EventType.Droppable=true` 表示事件可被内存控制丢弃。参考：`utils/dynstream/interfaces.go:41-48`。
- ReleasePath：dynstream 反馈类型，表示释放/丢弃某 path 的队列事件。参考：`utils/dynstream/interfaces.go:281-289`。
