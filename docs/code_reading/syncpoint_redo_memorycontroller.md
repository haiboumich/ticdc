# SyncPoint / Redo 与 Memory Controller 关联分析（代码验证版）

说明：本文仅聚焦关联点与交互路径；Memory Controller（见[术语汇总小节](#c-terminology)）的内部算法与释放链路详见 `docs/code_reading/memorycontroller.md`。

目录：
- [A. 关联基线：Memory Controller 在新架构中的接入点](#sec-a-overview)
  - [A.1 EventCollector 启用 DynamicStream Memory Controller（主/redo）](#sec-a1-enable)
  - [A.2 配额注入：DispatcherManager -> DynamicStream](#sec-a2-quota)
  - [A.3 主/redo DynamicStream 合并：取最小可用内存](#sec-a3-min)
  - [A.4 EventService 扫描受限：available memory quota](#sec-a4-scan)
- [B. SyncPoint ↔ Memory Controller 的交互路径](#sec-b-syncpoint)
  - [B.1 SyncPoint 作为阻塞事件（NonBatchable）](#sec-b1-block)
  - [B.2 SyncPoint 生成频率对阻塞密度的影响](#sec-b2-frequency)
  - [B.3 MySQL Sink 写入路径与阻塞窗口](#sec-b3-sink)
- [C. Redo ↔ Memory Controller 的交互路径](#sec-c-redo)
  - [C.1 主/redo 内存合并导致“最小值限流”](#sec-c1-min)
  - [C.2 redoQuota 来自 MemoryQuota 的拆分比例](#sec-c2-quota)
  - [C.3 redoGlobalTs 对主链路缓存的影响](#sec-c3-cache)
- [D. 问题背景与待验证点](#sec-d-issues)
  - [D.1 GitHub issue/PR/branch 现有线索](#sec-d1-github)
- [E. 可配置项说明](#sec-e-config)
- [F. 术语汇总小节](#c-terminology)

<a id="sec-a-overview"></a>
## A. 关联基线：Memory Controller 在新架构中的接入点

summary：
- 关键交互路径：EventCollector（见[术语汇总小节](#c-terminology)） -> DynamicStream（见[术语汇总小节](#c-terminology)） -> CongestionControl（见[术语汇总小节](#c-terminology)） -> EventService（见[术语汇总小节](#c-terminology)）
  - Memory Controller（见[术语汇总小节](#c-terminology)）在主/redo DynamicStream 同时启用
  - available memory 由 EventCollector 下发并限制 EventService 扫描

时序图（简化）：
```
EventCollector
   | (DynamicStream metrics: main + redo)
   v
CongestionControl --(available memory)--> EventService --(scan limit)--> EventStore
```

<a id="sec-a1-enable"></a>
### A.1 EventCollector 启用 DynamicStream Memory Controller（主/redo）

summary：
- EventCollector 的主/redo DynamicStream 均打开 EnableMemoryControl
  - 这意味着主/redo 两条链路均会参与内存配额反馈

```golang
// downstreamadapter/eventcollector/helper.go:25-45
option := dynstream.NewOption()
option.EnableMemoryControl = true
...
module := "event-collector"
if isRedo { module = "event-collector-redo" }
stream := dynstream.NewParallelDynamicStream(module, eventsHandler, option)
```
说明：DynamicStream（见[术语汇总小节](#c-terminology)）启用 Memory Controller 后会向 EventCollector 输出反馈指标。

<a id="sec-a2-quota"></a>
### A.2 配额注入：DispatcherManager -> DynamicStream

summary：
- DispatcherManager（见[术语汇总小节](#c-terminology)）把 changefeed 侧 quota 注入到 DynamicStream
  - 主链路使用 sinkQuota，redo 链路使用 redoQuota

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager.go:450-478
appcontext.GetService[*eventcollector.EventCollector](appcontext.EventCollector).AddDispatcher(d, e.sinkQuota)
```

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:53-60,160
manager.redoQuota = totalQuota * consistentMemoryUsage.MemoryQuotaPercentage / 100
manager.sinkQuota = totalQuota - manager.redoQuota
...
appcontext.GetService[*eventcollector.EventCollector](appcontext.EventCollector).AddDispatcher(rd, e.redoQuota)
```
说明：sinkQuota/redoQuota 的拆分直接影响后续 CongestionControl 可用内存。

<a id="sec-a3-min"></a>
### A.3 主/redo DynamicStream 合并：取最小可用内存

summary：
- EventCollector 取主/redo 两条 DynamicStream 的最小 available memory
  - redo 链路紧张时会拉低主链路可用内存

```golang
// downstreamadapter/eventcollector/event_collector.go:600-645
for _, quota := range c.ds.GetMetrics().MemoryControl.AreaMemoryMetrics { ... }
for _, quota := range c.redoDs.GetMetrics().MemoryControl.AreaMemoryMetrics {
    // take minimum between main and redo streams
    ...
}
```
说明：这一步会将 redo 链路压力传导到主链路扫描能力。

<a id="sec-a4-scan"></a>
### A.4 EventService 扫描受限：available memory quota

summary：
- EventService 在扫描前检查 changefeed 与 dispatcher 的 available memory
  - 额度不足会跳过 scan，并发送 resolvedTs

```golang
// pkg/eventservice/event_broker.go:588-627
item, ok = status.availableMemoryQuota.Load(remoteID)
...
ok = allocQuota(available, uint64(sl.maxDMLBytes))
if !ok { ... skip scan ... }
if uint64(sl.maxDMLBytes) > task.availableMemoryQuota.Load() { ... skip scan ... }
```

```golang
// pkg/eventservice/event_broker.go:1184-1216
func (c *eventBroker) handleCongestionControl(from node.ID, m *event.CongestionControl) {
    ...
    changefeed.availableMemoryQuota.Store(from, atomic.NewUint64(available))
    dispatcher.availableMemoryQuota.Store(available)
}
```
说明：EventService（见[术语汇总小节](#c-terminology)）以 available memory 为前置条件，限制扫描负载。

<a id="sec-b-syncpoint"></a>
## B. SyncPoint ↔ Memory Controller 的交互路径

summary：
- SyncPointEvent（见[术语汇总小节](#c-terminology)）是阻塞事件
  - 会降低 DynamicStream 的吞吐，从而触发 Memory Controller 限流

<a id="sec-b1-block"></a>
### B.1 SyncPoint 作为阻塞事件（NonBatchable）

summary：
- SyncPointEvent 标记为 NonBatchable
  - 需单独处理且阻塞后续事件

```golang
// downstreamadapter/eventcollector/helper.go:57-139
// If the event is a Sync Point event, we deal it as a multi-table DDL event.
case commonEvent.TypeSyncPointEvent:
    return dynstream.EventType{DataGroup: DataGroupSyncPoint, Property: dynstream.NonBatchable, Droppable: true}
```

```golang
// downstreamadapter/dispatcher/basic_dispatcher.go:679-697
case commonEvent.TypeSyncPointEvent:
    block = true
    syncPoint.AddPostFlushFunc(func() { wakeCallback() })
    d.DealWithBlockEvent(syncPoint)
```
说明：阻塞处理会抑制 DynamicStream 消费速度，可能导致内存控制收紧。

<a id="sec-b2-frequency"></a>
### B.2 SyncPoint 生成频率对阻塞密度的影响

summary：
- SyncPointEvent 插入在 DML/DDL/ResolvedTs 发送路径中
  - 频率由 SyncPointInterval 控制

```golang
// pkg/eventservice/event_broker.go:518-534
for d.enableSyncPoint && ts > d.nextSyncPoint.Load() { ... }
```
说明：SyncPointInterval 越小，阻塞事件密度越大。

<a id="sec-b3-sink"></a>
### B.3 MySQL Sink 写入路径与阻塞窗口

summary：
- SyncPoint 写入涉及 ddl_ts_v1 + syncpoint_v1 + tidb_external_ts
  - 下游事务耗时会拉长阻塞窗口

```golang
// pkg/sink/mysql/mysql_writer.go:169-203
FlushDDLTsPre(event) -> SendSyncPointEvent(event) -> FlushDDLTs(event)
```

```golang
// pkg/sink/mysql/mysql_writer_for_syncpoint.go:47-131
insert ignore into tidb_cdc.syncpoint_v1 ...
set global tidb_external_ts = ...
```
说明：写入路径越慢，DynamicStream 越容易因阻塞累积 pending。

<a id="sec-c-redo"></a>
## C. Redo ↔ Memory Controller 的交互路径

summary：
- redo 链路与主链路共享 Memory Controller 的“最小值”
  - redo 压力会降低主链路可用内存

<a id="sec-c1-min"></a>
### C.1 主/redo 内存合并导致“最小值限流”

summary：
- EventCollector 对主/redo available memory 取最小值
  - redo DynamicStream 紧张时，主链路 scan 被动减速

```golang
// downstreamadapter/eventcollector/event_collector.go:622-645
// take minimum between main and redo streams
```

<a id="sec-c2-quota"></a>
### C.2 redoQuota 来自 MemoryQuota 的拆分比例

summary：
- redoQuota = totalQuota * MemoryQuotaPercentage / 100
  - 会直接挤占主链路 sinkQuota

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:53-60
manager.redoQuota = totalQuota * consistentMemoryUsage.MemoryQuotaPercentage / 100
manager.sinkQuota = totalQuota - manager.redoQuota
```

<a id="sec-c3-cache"></a>
### C.3 redoGlobalTs 对主链路缓存的影响

summary：
- redoGlobalTs（见[术语汇总小节](#c-terminology)）落后时主链路事件缓存
  - 缓存不在 DynamicStream 队列内，需关注内存占用与 lag
- redoGlobalTs 的更新链路见 `docs/code_reading/redo.md` A.6.1

```golang
// downstreamadapter/dispatcher/event_dispatcher.go:135-142
if d.redoEnable && ... d.redoGlobalTs.Load() < dispatcherEvents[len(dispatcherEvents)-1].Event.GetCommitTs() {
    d.cache(dispatcherEvents, wakeCallback)
    return true
}
```

<a id="sec-d-issues"></a>
## D. 问题背景与待验证点

summary：
- 现状问题（调研输入）
  - Memory Controller + SyncPoint 组合后出现严重性能劣化（lag 持续上升）
  - redo 功能缺乏规模化测试，性能/稳定性未知
  - 现有公开线索集中在 SyncPoint 阻塞、memory quota 扫描策略与 redo lag

待验证点（基于代码路径推测）：
- NonBatchable 阻塞放大：SyncPointEvent 阻塞导致 DynamicStream pending 增大，触发 scan 限流。
- 最小值限流：redo DynamicStream 可用内存偏低时，主链路 scan 能力下降。
- redoGlobalTs 缓存：主链路事件缓存可能造成“内存控制已限流但 lag 继续上升”。

<a id="sec-d1-github"></a>
### D.1 GitHub issue/PR/branch 现有线索

summary：
- SyncPoint 与 Memory Controller 相关问题已有记录
  - 典型问题包括 SyncPoint 阻塞导致 changefeed 卡住、memory quota 扫描策略导致 dispatcher 饥饿
- Redo 相关问题集中在性能 lag 与稳定性场景
  - 公开 issue 多为 redo lag 或网络抖动下的稳定性问题

已知 issue（按主题分组）：
- Memory Controller + SyncPoint 组合问题
  - #1166（已关闭，2025-09-22）：SyncPoint 事件遇到 Memory Controller 暂停 dispatcher 导致 changefeed 卡住，内存持续增长。https://github.com/pingcap/ticdc/issues/1166
  - #4006（开放，2026-01-15）：重启后 SyncPoint 收集不完整导致 advancement stall。https://github.com/pingcap/ticdc/issues/4006
  - #4172（开放，2026-02-09）：eventBroker memory quota 扫描过多导致 dispatcher 饥饿，提出 memory-aware scan window。https://github.com/pingcap/ticdc/issues/4172

- Redo 功能与性能/稳定性问题
  - #1061（开放，2025-12-03）：redo 支持的功能清单与待改进项（含 scheduler 性能）。https://github.com/pingcap/ticdc/issues/1061
  - #3957（开放，2026-01-08）：启用 redo log 后 changefeed lag 高于未启用。https://github.com/pingcap/ticdc/issues/3957
  - #2572（已关闭，2025-10-11）：redo log + S3 网络抖动导致 changefeed 卡住。https://github.com/pingcap/ticdc/issues/2572

已知 PR（相关功能/修复）：
- Memory Controller / SyncPoint 相关
  - #362（已合并，2024-10-12）：引入 Memory Controller。https://github.com/pingcap/ticdc/pull/362
  - #1960（已合并，2025-09-07）：eventbroker 修复 reset dispatcher 后缺失 SyncPoint。https://github.com/pingcap/ticdc/pull/1960
  - #1157（已合并，2025-03-27）：支持 batch sync point event。https://github.com/pingcap/ticdc/pull/1157

- Redo 相关
  - #3704（开放，2026-02-11）：mysql/redo sink schema/table routing 支持。https://github.com/pingcap/ticdc/pull/3704
  - #2167（已合并，2025-11-28）：redo apply 重写。https://github.com/pingcap/ticdc/pull/2167
  - #1461（已合并，2025-06-30）：redo sink 引入。https://github.com/pingcap/ticdc/pull/1461

已知分支（Active 分支列表中与 memory quota 相关）：
- ldz/fix-memory-quota1126
- ldz/hack-memory-quota
- ldz/optimze-memory-quota0112
- ldz/test-memory-quota0127
- ldz/test-memory-quota012702

<a id="sec-e-config"></a>
## E. 可配置项说明

summary：
- 关键配置集中在 ReplicaConfig 与 ConsistentConfig
  - MemoryQuota 决定总配额
  - SyncPointInterval/Retention 决定阻塞密度
  - Consistent.MemoryUsage 决定 redoQuota 比例

#### E.1 默认值与关键字段
```golang
// pkg/config/replica_config.go:46-53
EnableSyncPoint:    util.AddressOf(false)
SyncPointInterval:  util.AddressOf(10 * time.Minute)
SyncPointRetention: util.AddressOf(24 * time.Hour)
```

```golang
// pkg/config/replica_config.go:84-96
Consistent: &ConsistentConfig{
    Level:                 util.AddressOf("none"),
    MaxLogSize:            util.AddressOf(redo.DefaultMaxLogSize),
    FlushIntervalInMs:     util.AddressOf(int64(redo.DefaultFlushIntervalInMs)),
    MetaFlushIntervalInMs: util.AddressOf(int64(redo.DefaultMetaFlushIntervalInMs)),
    EncodingWorkerNum:     util.AddressOf(redo.DefaultEncodingWorkerNum),
    FlushWorkerNum:        util.AddressOf(redo.DefaultFlushWorkerNum),
    Storage:               util.AddressOf(""),
    UseFileBackend:        util.AddressOf(false),
    Compression:           util.AddressOf(""),
    MemoryUsage: &ConsistentMemoryUsage{
        MemoryQuotaPercentage: 50,
    },
}
```

#### E.2 可配置方式
```golang
// cmd/cdc/cli/cli_changefeed_create.go:60-65
cmd.PersistentFlags().StringVar(&o.configFile, "config", "", "Path of the configuration file")
```

#### E.3 生效位置
```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:37-60
// initRedoComponet uses ConsistentConfig to enable redo and split quota
```

<a id="c-terminology"></a>
## F. 术语汇总小节

summary：
- 关键术语与代码指针
  - 每个术语给出定义与定位点

- Memory Controller：DynamicStream 内部的内存控制机制。
  - 代码指针：`downstreamadapter/eventcollector/helper.go:25-31`

- DynamicStream：动态流式调度组件（dynstream）。
  - 代码指针：`downstreamadapter/eventcollector/helper.go:25-45`

- EventCollector：事件中转与分发组件，负责汇总可用内存并下发 congestion control。
  - 代码指针：`downstreamadapter/eventcollector/event_collector.go:600-645`

- CongestionControl：EventCollector 下发给 EventService 的可用内存控制消息。
  - 代码指针：`pkg/eventservice/event_broker.go:1184-1216`

- EventService：事件扫描与发送组件，按 available memory 限制 scan。
  - 代码指针：`pkg/eventservice/event_broker.go:588-627`

- DispatcherManager：dispatcher 的创建/管理入口，并负责 quota 拆分。
  - 代码指针：`downstreamadapter/dispatchermanager/dispatcher_manager.go:180-260`

- SyncPointEvent：阻塞事件类型，会降低 DynamicStream 吞吐。
  - 代码指针：`downstreamadapter/dispatcher/basic_dispatcher.go:679-697`

- redoQuota/sinkQuota：changefeed 内存配额拆分比例。
  - 代码指针：`downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:53-60`

- redoGlobalTs：主链路前进门槛，落后时缓存事件。
  - 代码指针：`downstreamadapter/dispatcher/event_dispatcher.go:135-142`
