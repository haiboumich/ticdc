# SyncPoint 代码分析

**TL;DR**：
- **Too Long; Didn't Read**：本文档详细说明 SyncPoint 的运行机制、配置项、与下游组件的交互
- **核心内容**：
  - SyncPoint 作为阻塞事件（NonBatchable）影响可用内存
  - MySQL Sink 写入路径使用 syncpoint_v1 + ddl_ts_v1
  - 与 Memory Controller 的交互见 `docs/code_reading/syncpoint_redo_memorycontroller.md`
- **关键结论**：
  - SyncPoint Event 会阻塞调度，降低并发吞吐
  - redo path 有独立的 DynamicStream 和内存控制
  - SyncPoint 的生成频率影响阻塞密度
- **适用场景**：理解 SyncPoint 如何影响主链路性能
- **阅读建议**：按目录阅读，重点关注配置项和 Sink 写入路径

---

说明：以下按“配置入口 -> 生成 -> 分发 -> 下游写入”的链路组织；每条记录包含 `文件:行号`、代码片段、说明。

目录：
- [A. SyncPoint 运行机制详解（入口 -> 生成 -> 下游写入）](#sec-a-overview)
 - [A.1 配置入口与校验（默认值/限制）](#sec-a1-config)
 - [A.2 SyncPointConfig 下发与共享（DispatcherManager -> SharedInfo）](#sec-a2-config-prop)
 - [A.3 起始 SyncPointTs 计算](#sec-a3-start-ts)
 - [A.4 Dispatcher 注册/重置时下发 SyncPoint 参数](#sec-a4-register)
 - [A.5 EventService 侧初始化与事件生成](#sec-a5-eventservice)
 - [A.6 EventCollector/DynamicStream 事件分组与阻塞语义](#sec-a6-ds)
 - [A.7 Dispatcher 侧处理逻辑（阻塞/跳过 redo）](#sec-a7-dispatcher)
 - [A.8 MySQL Sink 写入路径（syncpoint_v1 + ddl_ts_v1）](#sec-a8-sink)
 - [A.9 SyncPointEvent 结构与影响范围](#sec-a9-event)
- [B. 可配置项说明](#sec-b-config)
- [C. 术语汇总小节](#c-terminology)

<a id="sec-a-overview"></a>
## A. SyncPoint 运行机制详解（入口 -> 生成 -> 下游写入）

summary：
- 链路主线：[ReplicaConfig](#c-terminology) -> [EventService](#c-terminology) -> [EventCollector](#c-terminology) -> [Dispatcher](#c-terminology) -> MySQL [Sink](#c-terminology)
- 关键状态/策略
 - [SyncPointEvent](#c-terminology) 是 BlockEvent，会改变调度节奏。
 - 生成频率由 SyncPointInterval 决定，影响事件密度与阻塞频次。
- 范围与非目标
 - 覆盖 SyncPoint 的配置、生成、分发与下游写入链路。
- 不覆盖 redo/[Memory Controller](#c-terminology) 的交互细节（见 `docs/code_reading/redo.md` 与 `docs/code_reading/memorycontroller.md`）。
- 关键角色/组件
 - EventService 生成 SyncPointEvent，EventCollector 负责投递，Dispatcher 负责阻塞处理，MySQL Sink 负责写入。
- 可靠性与降级
 - redo mode 下跳过 SyncPointEvent（见 A.7）。
- 可观测性
 - 下游 `syncpoint_v1` 与 `ddl_ts_v1` 表记录为最直接的行为结果（见 A.8）。
- 关键假设/前置条件
 - 仅 MySQL/TiDB 下游可启用 SyncPoint（见 A.1 的校验逻辑）。

<a id="sec-a0-trigger-flow"></a>
### A.0 完整触发与传导链路

summary：
- **核心澄清**：SyncPoint 是 TiCDC 定期生成的，不是下游 TiDB 触发的
- **配置来源**：changefeed 创建时用户配置 `enable-sync-point = true`
- **生成时机**：EventService 扫描事件时，检测到当前事件的 ts > nextSyncPoint 时生成
- **关键组件**：ReplicaConfig → DispatcherManager → SharedInfo → Dispatcher → DispatcherRequest → EventService

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  SyncPoint 触发与传导链路                                                            │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  1. 配置入口（用户创建 changefeed 时）                                                │
│  ┌─────────────────────────────────────────────────────────────────────────────┐    │
│  │  # changefeed-config.toml                                                   │    │
│  │  enable-sync-point = true                                                   │    │
│  │  sync-point-interval = "10m"                                                │    │
│  │  sync-point-retention = "24h"                                               │    │
│  └─────────────────────────────────────────────────────────────────────────────┘    │
│                                      │                                              │
│                                      ▼                                              │
│  2. DispatcherManager 构造 SyncPointConfig                                          │
│  ┌─────────────────────────────────────────────────────────────────────────────┐    │
│  │  // downstreamadapter/dispatchermanager/dispatcher_manager.go:220-224       │    │
│  │  if cfConfig.EnableSyncPoint {                                              │    │
│  │      syncPointConfig = &syncpoint.SyncPointConfig{                          │    │
│  │          SyncPointInterval:  cfConfig.SyncPointInterval,                    │    │
│  │      }                                                                      │    │
│  │  }                                                                          │    │
│  │  manager.sharedInfo = dispatcher.NewSharedInfo(..., syncPointConfig, ...)   │    │
│  └─────────────────────────────────────────────────────────────────────────────┘    │
│                                      │                                              │
│                                      ▼                                              │
│  3. Dispatcher 从 SharedInfo 获取配置，向 EventService 注册                          │
│  ┌─────────────────────────────────────────────────────────────────────────────┐    │
│  │  // downstreamadapter/eventcollector/dispatcher_stat.go:659-684             │    │
│  │  DispatcherRequest{                                                         │    │
│  │      EnableSyncPoint:   d.target.EnableSyncPoint(),                         │    │
│  │      SyncPointInterval: uint64(syncPointInterval.Seconds()),                │    │
│  │      SyncPointTs: syncpoint.CalculateStartSyncPointTs(startTs, ...),        │    │
│  │  }                                                                          │    │
│  └─────────────────────────────────────────────────────────────────────────────┘    │
│                                      │                                              │
│                                      ▼                                              │
│  4. EventService 保存配置到 dispatcherStat                                          │
│  ┌─────────────────────────────────────────────────────────────────────────────┐    │
│  │  // pkg/eventservice/dispatcher_stat.go:159-162                             │    │
│  │  dispStat.enableSyncPoint = true                                            │    │
│  │  dispStat.nextSyncPoint.Store(info.GetSyncPointTs())  // 第一个 ts           │    │
│  │  dispStat.syncPointInterval = info.GetSyncPointInterval()                   │    │
│  └─────────────────────────────────────────────────────────────────────────────┘    │
│                                      │                                              │
│                                      ▼                                              │
│  5. EventService 扫描事件时，按需插入 SyncPoint event                                │
│  ┌─────────────────────────────────────────────────────────────────────────────┐    │
│  │  // pkg/eventservice/event_broker.go:206-212                                │    │
│  │  for each DML event:                                                        │    │
│  │      if dml.commitTs > nextSyncPoint:  // 当前事件 ts 超过了                  │    │
│  │          emitSyncPointEvent(nextSyncPoint)  // 先发 SyncPoint                │    │
│  │          nextSyncPoint += interval           // 更新下一个                    │    │
│  │      sendDML(dml)                            // 再发 DML                      │    │
│  └─────────────────────────────────────────────────────────────────────────────┘    │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

**时间线示例：**

```
时间轴（interval = 10min）：
├────────┼────────┼────────┼────────┼────────┤
0       10min    20min    30min    40min
         ↑        ↑        ↑        ↑
      SyncPoint SyncPoint SyncPoint SyncPoint
       ts=T1     ts=T2     ts=T3     ts=T4

事件流（某个 dispatcher）：
DML(ts=5min) → DML(ts=8min) → SyncPoint(ts=T1=10min) → DML(ts=12min) → ...
                                    ↑
                          EventService 检测到 8min < T1 < 12min
                          在 DML(ts=12min) 之前插入 SyncPoint
```

<a id="sec-a1-config"></a>
### A.1 配置入口与校验（默认值/限制）

summary：
- 默认值与最小约束
 - 默认关闭；interval=10m；retention=24h
 - 最小 interval=30s；最小 retention=1h；仅允许 MySQL/TiDB 下游

#### A.1.1 默认值/最小值
```golang
// pkg/config/replica_config.go:34-53
const (
  minSyncPointInterval = time.Second * 30
  minSyncPointRetention = time.Hour * 1
)
var defaultReplicaConfig = &ReplicaConfig{
  EnableSyncPoint:  util.AddressOf(false),
  SyncPointInterval: util.AddressOf(10 * time.Minute),
  SyncPointRetention: util.AddressOf(24 * time.Hour),
}
```
说明：默认值与最小约束定义在 [ReplicaConfig](#c-terminology)。

#### A.1.2 校验逻辑
```golang
// pkg/config/replica_config.go:268-306
if util.GetOrZero(c.EnableSyncPoint) {
  if !IsMySQLCompatibleScheme(GetScheme(sinkURI)) {
    return cerror.ErrInvalidReplicaConfig.
      FastGenByArgs("The SyncPoint must be disabled when the downstream is not tidb or mysql")
  }
  if c.SyncPointInterval != nil && *c.SyncPointInterval < minSyncPointInterval { ... }
  if c.SyncPointRetention != nil && *c.SyncPointRetention < minSyncPointRetention { ... }
}
```
说明：SyncPoint 仅支持 MySQL/TiDB 下游；并在配置阶段强制 interval/retention 的最小值。

<a id="sec-a2-config-prop"></a>
### A.2 SyncPointConfig 下发与共享（DispatcherManager -> SharedInfo）

summary：
- [SharedInfo](#c-terminology) 作为 Dispatcher 共享配置容器
 - SyncPointConfig 仅在 EnableSyncPoint=true 时构造并注入

调用链：
- [DispatcherManager](#c-terminology) 创建
 - 从 changefeed config 构造 SyncPointConfig
  - 注入 SharedInfo

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager.go:219-255
var syncPointConfig *syncpoint.SyncPointConfig
if cfConfig.EnableSyncPoint {
  syncPointConfig = &syncpoint.SyncPointConfig{
    SyncPointInterval: cfConfig.SyncPointInterval,
    SyncPointRetention: cfConfig.SyncPointRetention,
  }
}
manager.sharedInfo = dispatcher.NewSharedInfo(
  ...,
  syncPointConfig,
  ...,
)
```
说明：[SharedInfo](#c-terminology) 负责把 SyncPoint 配置分发给所有 Dispatcher。

<a id="sec-a3-start-ts"></a>
### A.3 起始 SyncPointTs 计算

summary：
- 起始 SyncPointTs 向上取整
 - startTs 不整除 interval 或逻辑位不为 0 时，向上取整
 - `skipSyncpointAtStartTs` 为 true 且正好对齐时，跳过首个

```golang
// downstreamadapter/syncpoint/sync_point.go:22-38
func CalculateStartSyncPointTs(startTs uint64, syncPointInterval time.Duration, skipSyncpointAtStartTs bool) uint64 {
  if syncPointInterval == time.Duration(0) {
    return 0
  }
  k := oracle.GetTimeFromTS(startTs).Sub(time.Unix(0, 0)) / syncPointInterval
  if oracle.GetTimeFromTS(startTs).Sub(time.Unix(0, 0))%syncPointInterval != 0 || oracle.ExtractLogical(startTs) != 0 {
    k += 1
  } else if skipSyncpointAtStartTs {
    k += 1
  }
  return oracle.GoTimeToTS(time.Unix(0, 0).Add(k * syncPointInterval))
}
```
说明：起始 SyncPointTs 由 EventCollector 计算并下发给 EventService（见 A.4）。

<a id="sec-a4-register"></a>
### A.4 Dispatcher 注册/重置时下发 SyncPoint 参数

summary：
- EventCollector 在 DispatcherRequest 中携带 SyncPoint 相关字段
 - 注册与重置都会下发 Enable/Interval/Ts
 - 重置时仅在 resetTs==startTs 时沿用 skipSyncpointAtStartTs

```golang
// downstreamadapter/eventcollector/dispatcher_stat.go:659-708
func (d *dispatcherStat) newDispatcherRegisterRequest(serverId string, onlyReuse bool) *messaging.DispatcherRequest {
  startTs := d.target.GetStartTs()
  syncPointInterval := d.target.GetSyncPointInterval()
  return &messaging.DispatcherRequest{
    DispatcherRequest: &eventpb.DispatcherRequest{
      ...
      EnableSyncPoint:  d.target.EnableSyncPoint(),
      SyncPointInterval: uint64(syncPointInterval.Seconds()),
      SyncPointTs:    syncpoint.CalculateStartSyncPointTs(startTs, syncPointInterval, d.target.GetSkipSyncpointAtStartTs()),
      ...
    },
  }
}
```
说明：[EventCollector](#c-terminology) 向 EventService 发送 DispatcherRequest，SyncPoint 的起始值由此传入。

<a id="sec-a5-eventservice"></a>
### A.5 EventService 侧初始化与事件生成

summary：
- EventService 在 dispatcherStat 中保存 SyncPoint 状态
 - `enableSyncPoint/nextSyncPoint/interval` 用于驱动事件生成
- 生成发生在 DML/DDL/ResolvedTs 发送路径上
 - 每次发送前都会调用 `emitSyncPointEventIfNeeded`

#### A.5.1 初始化
```golang
// pkg/eventservice/dispatcher_stat.go:159-163
if info.SyncPointEnabled() {
  dispStat.enableSyncPoint = true
  dispStat.nextSyncPoint.Store(info.GetSyncPointTs())
  dispStat.syncPointInterval = info.GetSyncPointInterval()
}
```

#### A.5.2 生成与发送
```golang
// pkg/eventservice/event_broker.go:513-534
func (c *eventBroker) emitSyncPointEventIfNeeded(ts uint64, d *dispatcherStat, remoteID node.ID) {
  for d.enableSyncPoint && ts > d.nextSyncPoint.Load() {
    commitTs := d.nextSyncPoint.Load()
    d.nextSyncPoint.Store(oracle.GoTimeToTS(oracle.GetTimeFromTS(commitTs).Add(d.syncPointInterval)))
    e := event.NewSyncPointEvent(d.id, commitTs, d.seq.Add(1), d.epoch)
    syncPointEvent := newWrapSyncPointEvent(remoteID, e)
    c.getMessageCh(d.messageWorkerIndex, common.IsRedoMode(d.info.GetMode())) <- syncPointEvent
  }
}
```
说明：SyncPointEvent 在 [EventService](#c-terminology) 侧生成，按 dispatcher 的 message worker 发送。

<a id="sec-a6-ds"></a>
### A.6 EventCollector/DynamicStream 事件分组与阻塞语义

summary：
- SyncPointEvent 被标记为 NonBatchable
 - 同时被注释为“multi-table DDL”语义，要求单独处理

```golang
// downstreamadapter/eventcollector/helper.go:48-64,127-139
// If the event is a Sync Point event, we deal it as a multi-table DDL event.
// For DDL event and Sync Point Event, we should handle them singlely.
case commonEvent.TypeSyncPointEvent:
  return dynstream.EventType{DataGroup: DataGroupSyncPoint, Property: dynstream.NonBatchable, Droppable: true}
```
说明：[DynamicStream](#c-terminology) 对 SyncPointEvent 使用 NonBatchable 分组，确保单条阻塞处理。

<a id="sec-a7-dispatcher"></a>
### A.7 Dispatcher 侧处理逻辑（阻塞/跳过 redo）

summary：
- SyncPointEvent 为阻塞事件
 - 触发 `DealWithBlockEvent`，写入完成后唤醒
- redo mode 下直接跳过

```golang
// downstreamadapter/dispatcher/basic_dispatcher.go:679-697
case commonEvent.TypeSyncPointEvent:
  if common.IsRedoMode(d.GetMode()) {
    continue
  }
  block = true
  syncPoint := event.(*commonEvent.SyncPointEvent)
  syncPoint.AddPostFlushFunc(func() { wakeCallback() })
  d.DealWithBlockEvent(syncPoint)
```
说明：[Dispatcher](#c-terminology) 对 SyncPointEvent 采用阻塞处理策略，并在 redo 模式跳过。

补充：初始化阶段允许首个 syncpoint 与 startTs 相同。
```golang
// downstreamadapter/dispatcher/basic_dispatcher.go:449-459
// the first syncpoint event can be same as startTs
case commonEvent.TypeSyncPointEvent:
  if event.GetCommitTs() >= d.startTs {
    return true
  }
```

<a id="sec-a8-sink"></a>
### A.8 MySQL Sink 写入路径（syncpoint_v1 + ddl_ts_v1）

summary：
- 写入顺序：FlushDDLTsPre -> SendSyncPointEvent -> FlushDDLTs
 - 同一事务内写入 syncpoint_v1 与 tidb_external_ts
 - ddl_ts_v1 使用保留 row 避免 O(table_count) 更新

调用链：
- Dispatcher 触发 BlockEvent
 - MySQL Writer FlushSyncPointEvent
  - FlushDDLTsPre
  - SendSyncPointEvent
  - FlushDDLTs

```golang
// pkg/sink/mysql/mysql_writer.go:169-203
func (w *Writer) FlushSyncPointEvent(event *commonEvent.SyncPointEvent) error {
  if !w.syncPointTableInit { ... createSyncTable ... }
  err := w.FlushDDLTsPre(event)
  ...
  err = w.SendSyncPointEvent(event)
  ...
  err = w.FlushDDLTs(event)
  return err
}
```

```golang
// pkg/sink/mysql/mysql_writer_for_syncpoint.go:47-131
func (w *Writer) SendSyncPointEvent(event *commonEvent.SyncPointEvent) error {
  tx, err := w.db.BeginTx(w.ctx, nil)
  row := tx.QueryRow("select @@tidb_current_ts")
  ...
  query := fmt.Sprintf("insert ignore into %s.%s ...", ...)
  ...
  query = fmt.Sprintf("set global tidb_external_ts = %s", secondaryTs)
  ...
  return tx.Commit()
}
```

```golang
// pkg/sink/mysql/mysql_writer_for_ddl_ts.go:30-147
// Syncpoint should not update ddl_ts for all tables... Instead, we only update a single reserved row
if event.GetType() == commonEvent.TypeSyncPointEvent {
  tableIds = []int64{syncPointMetaTableID}
  isSyncpoint = "1"
}
```
说明：SyncPointEvent 会写入 `syncpoint_v1` 与 `ddl_ts_v1`，并维护 `tidb_external_ts`。

<a id="sec-a9-event"></a>
### A.9 SyncPointEvent 结构与影响范围

summary：
- SyncPointEvent 是 BlockEvent，影响范围为全表
 - `GetBlockedTables` 返回 InfluenceTypeAll

```golang
// pkg/common/event/sync_point_event.go:29-88
// Implement Event / FlushEvent / BlockEvent interface
// GetBlockedTables returns InfluenceTypeAll
func (e *SyncPointEvent) GetBlockedTables() *InfluencedTables {
  return &InfluencedTables{InfluenceType: InfluenceTypeAll}
}
```
说明：SyncPointEvent 以全表为影响范围，用于下游一致性标记。

<a id="sec-b-config"></a>
## B. 可配置项说明

summary：
- 配置入口与生效位置
 - 通过 changefeed config 文件（`--config`）或 API 下发 `ReplicaConfig`
 - 生效点：DispatcherManager 构造 SharedInfo 与 EventService 生成 SyncPointEvent

#### B.1 配置项与默认值
- `enable-sync-point`：默认 `false`（见 A.1）
- `sync-point-interval`：默认 `10m`，最小 `30s`（见 A.1）
- `sync-point-retention`：默认 `24h`，最小 `1h`（见 A.1）

```golang
// pkg/config/replica_config.go:149-163
EnableSyncPoint  *bool `toml:"enable-sync-point" json:"enable-sync-point,omitempty"`
SyncPointInterval *time.Duration `toml:"sync-point-interval" json:"sync-point-interval,omitempty"`
SyncPointRetention *time.Duration `toml:"sync-point-retention" json:"sync-point-retention,omitempty"`
```

#### B.2 可配置方式
```golang
// cmd/cdc/cli/cli_changefeed_create.go:60-65
cmd.PersistentFlags().StringVar(&o.configFile, "config", "", "Path of the configuration file")
```
说明：通过 `changefeed create --config` 指定 TOML 配置文件，字段落在 `ReplicaConfig` 中。

#### B.3 生效位置
```golang
// downstreamadapter/dispatchermanager/dispatcher_manager.go:219-255
// SyncPointConfig -> SharedInfo -> Dispatcher
```
说明：配置在 DispatcherManager 构建时注入 SharedInfo，最终影响 EventService 生成 SyncPointEvent。

<a id="c-terminology"></a>
## C. 术语汇总小节

summary：
- 关键术语与代码指针
 - 每个术语给出定义与定位点

- SyncPointEvent：用于标记一致性边界的 BlockEvent。
 - 代码指针：`pkg/common/event/sync_point_event.go:29-88`

- EventService：事件扫描/生成的服务端组件（本文关注其生成 SyncPointEvent 的逻辑）。
 - 代码指针：`pkg/eventservice/event_broker.go:513-534`

- EventCollector：事件中转与分发组件，向 DynamicStream 投递事件。
 - 代码指针：`downstreamadapter/eventcollector/event_collector.go:105-129`

- DynamicStream：动态流式调度组件（dynstream），负责队列/分组/阻塞语义。
 - 代码指针：`downstreamadapter/eventcollector/helper.go:25-45`

- Dispatcher：下游事件处理单元，负责阻塞事件处理与写入。
 - 代码指针：`downstreamadapter/dispatcher/basic_dispatcher.go:679-697`

- SharedInfo：Dispatcher 的共享配置容器，包含 SyncPointConfig。
 - 代码指针：`downstreamadapter/dispatcher/basic_dispatcher_info.go:20-110`

- ReplicaConfig：changefeed 侧的复制配置对象，包含 SyncPoint 相关配置。
 - 代码指针：`pkg/config/replica_config.go:120-170`

- DispatcherManager：dispatcher 的创建/管理入口，并构造 SharedInfo。
 - 代码指针：`downstreamadapter/dispatchermanager/dispatcher_manager.go:180-260`

- MySQL Sink：MySQL/TiDB 下游写入实现，负责 FlushSyncPointEvent。
 - 代码指针：`pkg/sink/mysql/mysql_writer.go:169-203`

- Memory Controller：DynamicStream 内部的内存控制机制（本文仅做边界说明）。
 - 代码指针：`downstreamadapter/eventcollector/helper.go:25-31`

- syncpoint_v1：下游 TiDB/MySQL 的 syncpoint 记录表。
 - 代码指针：`pkg/sink/mysql/mysql_writer_for_syncpoint.go:31-44`

- ddl_ts_v1（syncpoint meta row）：DDL/SyncPoint 状态表，syncpoint 使用保留 row。
 - 代码指针：`pkg/sink/mysql/mysql_writer_for_ddl_ts.go:30-88`

---

<a id="d-faq"></a>
## D. FAQ

summary：
- 汇总 SyncPoint 相关的常见问题与解答
- 涵盖 ts 计算、一致性保证、协调机制等核心问题

<a id="d1-syncpoint-ts-calculation"></a>
### D.1 SyncPoint ts 是怎么计算的？为什么不取所有 dispatcher 的最低值？

**问题**：SyncPoint 的 ts 是 `startTs + interval * n`，这个 n 是怎么算的？为什么不直接取所有 dispatcher 的最低 checkpointTs？

**解答**：

**1. SyncPoint ts 的计算方式**

```go
// downstreamadapter/syncpoint/sync_point.go:28-39
func CalculateStartSyncPointTs(startTs uint64, syncPointInterval time.Duration, skipSyncpointAtStartTs bool) uint64 {
    // k = (startTs 对应的时间 - Unix epoch) / interval
    k := oracle.GetTimeFromTS(startTs).Sub(time.Unix(0, 0)) / syncPointInterval

    // 如果 startTs 不整除 interval，向上取整
    if oracle.GetTimeFromTS(startTs).Sub(time.Unix(0, 0))%syncPointInterval != 0 {
        k += 1
    }

    // 返回 Unix epoch + k * interval
    return oracle.GoTimeToTS(time.Unix(0, 0).Add(k * syncPointInterval))
}
```

**计算示例（interval = 10 分钟）：**

```
假设 startTs = 09:03:45

1. k = (09:03:45 - 00:00:00) / 10min = 54（对应 09:00:00）
2. 09:03:45 % 10min = 3min 45s ≠ 0 → 向上取整，k = 55
3. 第一个 SyncPoint ts = 00:00:00 + 55 * 10min = 09:10:00
4. 后续：09:10:00 → 09:20:00 → 09:30:00 → ...

【关键】所有 dispatcher 都使用相同的时间起点（Unix epoch）
       所以同一个 interval 边界对所有 dispatcher 都是相同的 ts
```

**2. 为什么不取所有 dispatcher 的最低值？**

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  如果取最低值，就没有一致性时间标记了                                                  │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  当前设计（预先确定 ts）：                                                           │
│  • Dispatcher A 收到 SyncPoint ts=100 → 写入下游                                    │
│  • Dispatcher B 收到 SyncPoint ts=100 → 写入下游                                    │
│  • Dispatcher C 收到 SyncPoint ts=100 → 写入下游                                    │
│  • 结果：所有表的 primary_ts 都是 100，一致性有保证                                   │
│                                                                                     │
│  如果取最低值（动态计算）：                                                          │
│  • Dispatcher A checkpoint=80 → SyncPoint ts=80                                     │
│  • Dispatcher B checkpoint=60 → SyncPoint ts=60                                     │
│  • Dispatcher C checkpoint=90 → SyncPoint ts=90                                     │
│  • 问题：每个表的 primary_ts 不一样！用户无法用统一的 ts 查询一致性快照              │
│                                                                                     │
│  【SyncPoint 的目的】在某个特定的、预先确定的时间点，所有表的状态是一致的             │
│  如果取最低值，每个表各自独立，无法建立一致性快照                                     │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

<a id="d2-syncpoint-insertion"></a>
### D.2 SyncPoint event 是怎么给到 dispatcher 的？为什么写入后就能保证之前的数据都执行了？

**问题**：SyncPoint event 是怎么插入到事件流中的？和前面的 path 是什么关系？为什么写入后就能认为之前的数据都执行了？

**解答**：

**1. SyncPoint event 插入到事件流中**

```go
// pkg/eventservice/event_broker.go:206-212
for each DML event:
    if dml.commitTs > nextSyncPoint:  // 当前事件的 ts 超过了下一个 SyncPoint
        // 先发送 SyncPoint event
        emitSyncPointEvent(nextSyncPoint)
        nextSyncPoint += interval
    // 然后发送 DML event
    sendDML(dml)
```

**事件流示例：**

```
DML(ts=95) → SyncPoint(ts=100) → DML(ts=105) → DML(ts=115) → SyncPoint(120)
               ↑
         插入到 95 和 105 之间
```

**2. 为什么写入后就能保证之前的数据都执行了？**

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  关键保证：EventService 按 commitTs 顺序发送事件                                      │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  1. EventService 扫描 EventStore 时，按 commitTs 升序返回事件                        │
│                                                                                     │
│  2. 当 SyncPoint(ts=100) 发送给 dispatcher 时：                                      │
│     - EventStore 中所有 commitTs < 100 的事件都已经被扫描到                           │
│     - 这些事件要么已经发送给 dispatcher，要么在 SyncPoint 之前发送                    │
│                                                                                     │
│  3. dispatcher 处理 SyncPoint 时：                                                   │
│     - SyncPoint 是 BlockEvent，必须先写入下游才能继续                                 │
│     - 写入 SyncPoint 意味着该 dispatcher 已经处理完所有 ts < 100 的事件               │
│                                                                                     │
│  【保证来源于】SyncPoint 插入在事件流中的正确位置 + BlockEvent 的阻塞特性              │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

<a id="d3-syncpoint-table"></a>
### D.3 syncpoint_v1 表只有一行，每个 dispatcher 都写不会冲突吗？

**问题**：syncpoint_v1 表看起来只有一行记录，每个 dispatcher 都写入，是怎么处理的？

**解答**：

**syncpoint_v1 表是 changefeed 级别的，不是表（dispatcher）级别的！**

```sql
CREATE TABLE ticdc.syncpoint_v1 (
    ticdc_cluster_id varchar(255),
    changefeed varchar(255),
    primary_ts varchar(18),
    secondary_ts varchar(18),
    created_at timestamp,
    PRIMARY KEY (changefeed, primary_ts)  -- 主键是 changefeed + primary_ts
);
```

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  每个 dispatcher 写入相同的 primary_ts，但只会保留一行                                 │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  Dispatcher A (表1) ────┐                                                           │
│  Dispatcher B (表2) ────┼──→ INSERT IGNORE INTO syncpoint_v1 (changefeed, primary_ts)│
│  Dispatcher C (表3) ────┘       VALUES ('cf-1', '449739748248899584', ...)          │
│                                                                                     │
│  由于使用 INSERT IGNORE，相同的 (changefeed, primary_ts) 只会保留一行                 │
│                                                                                     │
│  【意义】                                                                            │
│  当这一行写入成功，意味着所有 dispatcher 都已经处理完这个 primary_ts 之前的数据        │
│  也就是：所有表在 primary_ts 这个时间点都达到了一致状态                                │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

<a id="d4-syncpoint-coordination"></a>
### D.4 每个 dispatcher 怎么知道要等其他的都写完？等待过程是什么？

**问题**：一个 dispatcher 写入 syncpoint 后，怎么知道要等其他的 dispatcher 都写完？是一直循环等待吗？怎么知道所有的都写进去了？

**解答**：

**核心：SyncPoint 有 Maintainer 协调机制，不是 dispatcher 自己判断的！**

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  SyncPoint 的完整协调流程                                                            │
├─────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                     │
│  1. 每个 Dispatcher 收到 SyncPoint 后                                               │
│     • shouldBlock(SyncPoint) → true（SyncPoint 总是 block）                         │
│     • reportBlockedEventToMaintainer() → 报告 WAITING 状态                          │
│                                                                                     │
│  2. Maintainer 收集所有 Dispatcher 的报告                                           │
│     • allDispatcherReported() 检查 rangeChecker.IsFullyCovered()                   │
│     • 对于 InfluenceTypeAll，需要所有 dispatcher 都报告                              │
│     • 选择一个 dispatcher 作为 writer（通常是 table trigger dispatcher）             │
│     • 发送 Action_Write 给 writer                                                   │
│                                                                                     │
│  3. Writer Dispatcher 执行写入                                                      │
│     • 收到 Action_Write → 写入 syncpoint_v1 表                                      │
│     • 写入完成后报告 DONE 状态                                                       │
│                                                                                     │
│  4. Maintainer 通知其他 Dispatcher                                                  │
│     • 收到 DONE 后，发送 Action_Pass 给所有其他 dispatcher                           │
│     • 其他 dispatcher 跳过写入，继续处理后续事件                                      │
│                                                                                     │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

**快的 dispatcher 不是"循环等待"，而是"被动等待通知"：**

```
Dispatcher A (快)            Dispatcher B (慢)
     │                            │
     │ 收到 SyncPoint            │
     │ 报告 WAITING              │
     │ ↓                         │
     │ 等待 maintainer 通知...    │  （不是循环，是被动等待）
     │                           │ 收到 SyncPoint
     │                           │ 报告 WAITING
     │                           │
     ↓                           ↓
┌─────────────────────────────────────────────────────────────────────────────────────┐
│  Maintainer 检测到 allDispatcherReported() = true                                    │
│  选择 A 作为 writer，发送 Action_Write → A 写入 syncpoint_v1                         │
│  发送 Action_Pass 给 B → B 跳过写入，继续处理                                         │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

**关键代码位置：**

```go
// 1. SyncPoint 总是 block
// basic_dispatcher.go:873-874
case commonEvent.TypeSyncPointEvent:
    return true

// 2. Block 后报告给 maintainer
// basic_dispatcher.go:1063-1090
func (d *BasicDispatcher) reportBlockedEventToMaintainer(event BlockEvent) {
    d.blockEventStatus.setBlockEvent(event, heartbeatpb.BlockStage_WAITING)
    // 发送状态给 maintainer
}

// 3. Maintainer 检查是否所有 dispatcher 都报告了
// barrier_event.go:322-329
func (be *BarrierEvent) allDispatcherReported() bool {
    return be.rangeChecker.IsFullyCovered()  // 检查所有表的 span 是否都已覆盖
}
```

<a id="d5-memory-syncpoint"></a>
### D.5 增加内存能避免 SyncPoint 问题吗？与 Memory Controller 的关系？

**问题**：如果内存足够大（比如 200G），是否能避免 SyncPoint 导致的性能问题？

**解答**：

**答案：不能完全避免，只能延迟问题出现。**

详见 `docs/code_reading/memorycontroller.md` 的 Q10 FAQ。核心结论：

| 问题 | 答案 |
|------|------|
| 内存够大能避免 SyncPoint 问题吗？ | **不能**，只能延迟。SyncPoint 的阻塞是业务层面的同步 |
| releasePath 和 SyncPoint 的关联？ | **恶性循环**：releasePath → 进度落后 → SyncPoint 阻塞 → 性能下降 |

**真正的解决方案**不是增加内存，而是让 Memory Controller 能感知各 dispatcher 的进度差异，避免慢 dispatcher 被过度限流导致 releasePath。详见 `docs/code_reading/syncpoint_redo_memorycontroller.md`。
