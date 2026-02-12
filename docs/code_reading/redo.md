# Redo 代码分析

说明：以下按“配置入口 -> 启用/分流 -> 分发 -> 写入 -> 元数据”的链路组织；每条记录包含 `文件:行号`、代码片段、说明。

目录：
- [A. Redo 运行机制详解（入口 -> 分发 -> 写入 -> 元数据）](#sec-a-overview)
 - [A.1 配置入口与校验（ConsistentConfig）](#sec-a1-config)
 - [A.2 启用 redo + 内存配额切分](#sec-a2-quota)
 - [A.3 Redo Dispatcher 创建与注册（EventCollector redo DynamicStream）](#sec-a3-dispatcher)
 - [A.4 Redo Sink 写入流程（DDL/DML）](#sec-a4-sink)
 - [A.5 Redo Meta 维护与上报](#sec-a5-meta)
 - [A.6 Redo 与主链路的进度耦合（redoGlobalTs）](#sec-a6-coupling)
 - [A.6.1 redoGlobalTs 更新链路（RedoMeta -> Maintainer -> DispatcherManager）](#sec-a6-1-update)
 - [A.6.2 缓存与回放细节（cacheEvents）](#sec-a6-2-cache)
 - [A.7 Redo 事件路由（EventService -> EventCollector）](#sec-a7-routing)
- [B. 可配置项说明](#sec-b-config)
- [C. 术语汇总小节](#c-terminology)

<a id="sec-a-overview"></a>
## A. Redo 运行机制详解（入口 -> 分发 -> 写入 -> 元数据）

summary：
- 链路主线：[ReplicaConfig](#c-terminology) -> [DispatcherManager](#c-terminology) -> Redo [Dispatcher](#c-terminology) -> Redo [Sink](#c-terminology) -> [RedoMeta](#c-terminology)
- 关键状态/策略
 - redo 开启后按内存占比拆分 changefeed 配额。
 - redoGlobalTs 会限制主链路事件前进。
- 范围与非目标
 - 覆盖 redo 启用/分流/写入/元数据上报链路。
 - 不覆盖 syncpoint 与 [Memory Controller](#c-terminology) 的交互细节（见 `docs/code_reading/syncpoint.md` 与 `docs/code_reading/memorycontroller.md`）。
- 关键角色/组件
 - DispatcherManager 负责启用与配额拆分，RedoDispatcher 负责写入前分发，RedoSink 负责落盘，RedoMeta 负责进度上报。
- 关键假设/前置条件
 - ConsistentConfig.level=eventual 才会启用 redo（见 A.1/A.2）。
- 可靠性与降级
 - ConsistentConfig.level=none 时不启用 redo，主链路按常规路径运行（见 A.1/A.2）。
- 可观测性
 - RedoResolvedTsProgressMessage 与 redoGlobalTs 提供进度与限流信号（见 A.5/A.6）。

时序图（简化）：
```
ReplicaConfig
  |
  v
DispatcherManager --(split quota)--> RedoDispatcher --(write)--> RedoSink --(flush)--> RedoMeta --(report resolved)--> Maintainer
                   |
                   +--(redoGlobalTs)--> EventDispatcher cache
```

<a id="sec-a1-config"></a>
### A.1 配置入口与校验（ConsistentConfig）

summary：
- redo 由 [ConsistentConfig](#c-terminology) 控制
 - `level=eventual` 表示启用 redo
 - 校验包含 flush 参数与 storage URI

```golang
// pkg/config/consistent.go:26-72
// ConsistentConfig represents replication consistency config for a changefeed.
// It is used by redo log functionality.
type ConsistentConfig struct {
  // Level is the consistency level, it can be `none` or `eventual`.
  // `eventual` means enable redo log.
  Level *string `toml:"level" json:"level,omitempty"`
  MaxLogSize *int64 `toml:"max-log-size" json:"max-log-size,omitempty"`
  FlushIntervalInMs *int64 `toml:"flush-interval" json:"flush-interval,omitempty"`
  MetaFlushIntervalInMs *int64 `toml:"meta-flush-interval" json:"meta-flush-interval,omitempty"`
  EncodingWorkerNum *int `toml:"encoding-worker-num" json:"encoding-worker-num,omitempty"`
  FlushWorkerNum *int `toml:"flush-worker-num" json:"flush-worker-num,omitempty"`
  Storage *string `toml:"storage" json:"storage,omitempty"`
  UseFileBackend *bool `toml:"use-file-backend" json:"use-file-backend,omitempty"`
  Compression *string `toml:"compression" json:"compression,omitempty"`
  FlushConcurrency *int `toml:"flush-concurrency" json:"flush-concurrency,omitempty"`
  MemoryUsage *ConsistentMemoryUsage `toml:"memory-usage" json:"memory-usage,omitempty"`
}
```

```golang
// pkg/config/consistent.go:74-119
if !redo.IsConsistentEnabled(util.GetOrZero(c.Level)) { return nil }
...
uri, err := storage.ParseRawURL(util.GetOrZero(c.Storage))
return redo.ValidateStorage(uri)
```
说明：[ConsistentConfig](#c-terminology)是 redo 功能的配置入口。

<a id="sec-a2-quota"></a>
### A.2 启用 redo + 内存配额切分

summary：
- initRedoComponet 决定是否启用 redo
 - 启用后创建 redoSink，并基于 `MemoryQuotaPercentage` 分配 redoQuota/sinkQuota

调用链：
- DispatcherManager initRedoComponet
 - 检查 ConsistentConfig.level
  - 计算 redoQuota
  - 创建 redoSink

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:37-60
if manager.config.Consistent == nil || !pkgRedo.IsConsistentEnabled(util.GetOrZero(manager.config.Consistent.Level)) {
  return nil
}
manager.RedoEnable = true
manager.redoSink = redo.New(ctx, changefeedID, manager.config.Consistent)
...
manager.redoQuota = totalQuota * consistentMemoryUsage.MemoryQuotaPercentage / 100
manager.sinkQuota = totalQuota - manager.redoQuota
```
说明：redoQuota 与 sinkQuota 共同来源于 changefeed 的 [MemoryQuota](#c-terminology)。

<a id="sec-a3-dispatcher"></a>
### A.3 Redo Dispatcher 创建与注册（EventCollector redo DynamicStream）

summary：
- RedoDispatcher 与普通 Dispatcher 同构，但 mode=Redo
 - 非 table-trigger redo dispatcher 会注册到 [EventCollector](#c-terminology) 的 redo [DynamicStream](#c-terminology)

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:140-161
rd := dispatcher.NewRedoDispatcher(
  id, tableSpans[idx], uint64(startTsList[idx]), schemaIds[idx],
  e.redoSchemaIDToDispatchers,
  false, // skipSyncpointAtStartTs
  scheduleSkipDMLAsStartTsList[idx],
  e.redoSink,
  e.sharedInfo,
)
if rd.IsTableTriggerDispatcher() {
  e.SetTableTriggerRedoDispatcher(rd)
} else {
  e.redoSchemaIDToDispatchers.Set(schemaIds[idx], id)
  appcontext.GetService[*eventcollector.EventCollector](appcontext.EventCollector).AddDispatcher(rd, e.redoQuota)
}
```
说明：redo dispatcher 的事件通过 [EventCollector](#c-terminology) 的 redo DynamicStream 处理。

<a id="sec-a4-sink"></a>
### A.4 Redo Sink 写入流程（DDL/DML）

summary：
- DDL 直接写入 ddlWriter
 - DML 先入 logBuffer，再由后台写入 dmlWriter

```golang
// downstreamadapter/sink/redo/sink.go:59-161
s := &Sink{ cfg: &writer.LogWriterConfig{...}, logBuffer: chann.NewUnlimitedChannelDefault[writer.RedoEvent](), ... }
...
ddlWriter, err := factory.NewRedoLogWriter(..., redo.RedoDDLLogFileType)
...
dmlWriter, err := factory.NewRedoLogWriter(..., redo.RedoRowLogFileType)
...
func (s *Sink) WriteBlockEvent(event commonEvent.BlockEvent) error {
  switch e := event.(type) {
  case *commonEvent.DDLEvent:
    return s.ddlWriter.WriteEvents(s.ctx, e)
  }
  return nil
}

func (s *Sink) AddDMLEvent(event *commonEvent.DMLEvent) {
  ...
  s.logBuffer.Push(&commonEvent.RedoRowEvent{...})
}
```
说明：Redo [Sink](#c-terminology) 通过 DDL/DML 分离写入。

<a id="sec-a5-meta"></a>
### A.5 Redo Meta 维护与上报

summary：
- Table-Trigger redo dispatcher 独占 meta
 - `SetRedoMeta` 启动 RedoMeta
 - DispatcherManager 周期上报 resolvedTs 到 Maintainer

```golang
// downstreamadapter/dispatcher/redo_dispatcher.go:89-118
func (rd *RedoDispatcher) SetRedoMeta(cfg *config.ConsistentConfig) {
  rd.redoMeta = redo.NewRedoMeta(rd.sharedInfo.changefeedID, rd.startTs, cfg)
  go func() { _ = rd.redoMeta.PreStart(ctx); _ = rd.redoMeta.Run(ctx) }()
}
```

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:285-311
logMeta := e.GetTableTriggerRedoDispatcher().GetFlushedMeta()
err := mc.SendCommand(
  messaging.NewSingleTargetMessage(
    e.GetMaintainerID(),
    messaging.MaintainerManagerTopic,
    &heartbeatpb.RedoResolvedTsProgressMessage{ ChangefeedID: e.changefeedID.ToPB(), ResolvedTs: logMeta.ResolvedTs },
  ))
```
说明：[RedoMeta](#c-terminology)是 redo 进度的权威来源。

<a id="sec-a6-coupling"></a>
### A.6 Redo 与主链路的进度耦合（redoGlobalTs）

summary：
- [redoGlobalTs](#c-terminology) 落后时，主链路事件会被缓存
 - 直到 redo 追上才继续 handleEvents
- redoGlobalTs 的更新来自 RedoMeta 上报并经 Maintainer 广播
 - 更新后会触发 EventDispatcher 回放缓存事件

```golang
// downstreamadapter/dispatcher/event_dispatcher.go:135-142
if d.redoEnable && len(dispatcherEvents) > 0 && d.redoGlobalTs.Load() < dispatcherEvents[len(dispatcherEvents)-1].Event.GetCommitTs() {
  d.cache(dispatcherEvents, wakeCallback)
  return true
}
```
说明：[redoGlobalTs](#c-terminology) 用于限制主链路前进，保证 redo 先行落盘。

<a id="sec-a6-1-update"></a>
#### A.6.1 redoGlobalTs 更新链路（RedoMeta -> Maintainer -> DispatcherManager）

summary：
- redoGlobalTs 由 table-trigger redo dispatcher 的 RedoMeta resolvedTs 驱动
 - DispatcherManager.collectRedoMeta 周期上报 RedoResolvedTsProgressMessage
- Maintainer.onRedoPersisted 广播 RedoResolvedTsForwardMessage 到 HeartbeatCollector
 - Handler 更新 redoGlobalTs，并触发所有 EventDispatcher.HandleCacheEvents

调用链（主干）：
- RedoDispatcher.GetFlushedMeta
 - DispatcherManager.collectRedoMeta -> RedoResolvedTsProgressMessage
  - Maintainer.onRedoPersisted -> RedoResolvedTsForwardMessage
   - HeartBeatCollector.RecvMessages -> redoResolvedTsForwardMessageDynamicStream
    - RedoResolvedTsForwardMessageHandler.Handle -> SetRedoResolvedTs -> HandleCacheEvents

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:285-311 (func collectRedoMeta)
logMeta := e.GetTableTriggerRedoDispatcher().GetFlushedMeta()
...
err := mc.SendCommand(
  messaging.NewSingleTargetMessage(
    e.GetMaintainerID(),
    messaging.MaintainerManagerTopic,
    &heartbeatpb.RedoResolvedTsProgressMessage{
      ChangefeedID: e.changefeedID.ToPB(),
      ResolvedTs:  logMeta.ResolvedTs,
    },
  ))
```

```golang
// maintainer/maintainer.go:524-535 (func onRedoPersisted)
if m.redoResolvedTs < req.ResolvedTs {
  m.redoResolvedTs = req.ResolvedTs
  for _, id := range m.bootstrapper.GetAllNodeIDs() {
    msgs = append(msgs, messaging.NewSingleTargetMessage(
      id, messaging.HeartbeatCollectorTopic,
      &heartbeatpb.RedoResolvedTsForwardMessage{ChangefeedID: req.ChangefeedID, ResolvedTs: m.redoResolvedTs},
    ))
  }
  m.sendMessages(msgs)
}
```

```golang
// downstreamadapter/dispatchermanager/helper.go:658-668 (func (*RedoResolvedTsForwardMessageHandler).Handle)
ok := dispatcherManager.SetRedoResolvedTs(msg.ResolvedTs)
if ok {
  dispatcherManager.dispatcherMap.ForEach(func(id common.DispatcherID, dispatcher *dispatcher.EventDispatcher) {
    dispatcher.HandleCacheEvents()
  })
}
```

```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:281-282 (func SetRedoResolvedTs)
return util.CompareAndMonotonicIncrease(&e.redoGlobalTs, resolvedTs)
```

<a id="sec-a6-2-cache"></a>
#### A.6.2 缓存与回放细节（cacheEvents）

summary：
- 缓存落在 EventDispatcher.cacheEvents（容量=1 的 channel）
 - 为避免动态流回收切片，cache 会复制 dispatcherEvents
- redoGlobalTs 更新后调用 HandleCacheEvents
 - 从缓存取出并重新执行 HandleEvents，解除阻塞后唤醒 wakeCallback

```golang
// downstreamadapter/dispatcher/event_dispatcher.go:34-48 (type EventDispatcher)
// cacheEvents is used to store events with a commit-ts greater than redoGlobalTs
cacheEvents struct {
  sync.Mutex
  events chan cacheEvents
}
```

```golang
// downstreamadapter/dispatcher/event_dispatcher.go:92-105 (func HandleCacheEvents)
select {
case cacheEvents, ok := <-d.cacheEvents.events:
  if !ok { return }
  block := d.HandleEvents(cacheEvents.events, cacheEvents.wakeCallback)
  if !block { cacheEvents.wakeCallback() }
default:
}
```

```golang
// downstreamadapter/dispatcher/event_dispatcher.go:107-121 (func cache)
cacheEvents := cacheEvents{
  events:    append(make([]DispatcherEvent, 0, len(dispatcherEvents)), dispatcherEvents...),
  wakeCallback: wakeCallback,
}
select {
case d.cacheEvents.events <- cacheEvents:
  ...
default:
  log.Panic("dispatcher cache events is full", ...)
}
```

<a id="sec-a7-routing"></a>
### A.7 Redo 事件路由（EventService -> EventCollector）

summary：
- EventService 根据 dispatcher mode 选择发送到 redo topic
 - EventCollector 独立消费 redo topic 并投递到 redo DynamicStream

```golang
// pkg/eventservice/event_broker.go:187-245
c.getMessageCh(d.messageWorkerIndex, common.IsRedoMode(d.info.GetMode())) <- newWrapBatchDMLEvent(...)
...
case c.getMessageCh(d.messageWorkerIndex, common.IsRedoMode(d.info.GetMode())) <- ddlEvent:
```

```golang
// downstreamadapter/eventcollector/event_collector.go:176-200
eventCollector.ds = NewEventDynamicStream(false)
eventCollector.redoDs = NewEventDynamicStream(true)
eventCollector.mc.RegisterHandler(messaging.EventCollectorTopic, eventCollector.MessageCenterHandler)
eventCollector.mc.RegisterHandler(messaging.RedoEventCollectorTopic, eventCollector.RedoMessageCenterHandler)
```
说明：[EventCollector](#c-terminology) 分离主/redo 两条 DynamicStream。

<a id="sec-b-config"></a>
## B. 可配置项说明

summary：
- 配置入口与生效位置
 - 通过 changefeed config 文件（`--config`）或 API 下发 `ReplicaConfig`
 - 生效点：initRedoComponet、RedoSink/RedoMeta 初始化

#### B.1 默认值与关键字段
```golang
// pkg/config/replica_config.go:84-96
Consistent: &ConsistentConfig{
  Level:         util.AddressOf("none"),
  MaxLogSize:      util.AddressOf(redo.DefaultMaxLogSize),
  FlushIntervalInMs:   util.AddressOf(int64(redo.DefaultFlushIntervalInMs)),
  MetaFlushIntervalInMs: util.AddressOf(int64(redo.DefaultMetaFlushIntervalInMs)),
  EncodingWorkerNum:   util.AddressOf(redo.DefaultEncodingWorkerNum),
  FlushWorkerNum:    util.AddressOf(redo.DefaultFlushWorkerNum),
  Storage:        util.AddressOf(""),
  UseFileBackend:    util.AddressOf(false),
  Compression:      util.AddressOf(""),
  MemoryUsage: &ConsistentMemoryUsage{
    MemoryQuotaPercentage: 50,
  },
}
```
说明：默认 `level=none`，即关闭 redo。

#### B.2 可配置方式
```golang
// cmd/cdc/cli/cli_changefeed_create.go:60-65
cmd.PersistentFlags().StringVar(&o.configFile, "config", "", "Path of the configuration file")
```

#### B.3 生效位置
```golang
// downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:37-60
// initRedoComponet uses ConsistentConfig to enable redo and split quota
```

<a id="c-terminology"></a>
## C. 术语汇总小节

summary：
- 关键术语与代码指针
 - 每个术语给出定义与定位点

- ConsistentConfig：redo 功能配置入口。
 - 代码指针：`pkg/config/consistent.go:26-72`

- ReplicaConfig：changefeed 侧的复制配置对象，包含 ConsistentConfig。
 - 代码指针：`pkg/config/replica_config.go:120-180`

- MemoryQuota：changefeed 内存配额配置字段，用于拆分 redoQuota/sinkQuota。
 - 代码指针：`pkg/config/replica_config.go:46-53`

- DispatcherManager：dispatcher 的创建/管理入口，并负责 redo 初始化与 quota 拆分。
 - 代码指针：`downstreamadapter/dispatchermanager/dispatcher_manager.go:100-180`

- RedoDispatcher：负责把事件写入 redo sink 的 dispatcher（mode=Redo）。
 - 代码指针：`downstreamadapter/dispatcher/redo_dispatcher.go:31-73`

- RedoSink：redo 日志写入实现（DDL/DML 分离）。
 - 代码指针：`downstreamadapter/sink/redo/sink.go:59-161`

- RedoMeta：redo 进度元数据（checkpoint/resolved）。
 - 代码指针：`downstreamadapter/sink/redo/meta.go:24-120`

- redoGlobalTs：主链路前进门槛，落后时缓存事件。
 - 代码指针：`downstreamadapter/dispatcher/event_dispatcher.go:135-142`

- Memory Controller：DynamicStream 内部的内存控制机制（本文仅做边界说明）。
 - 代码指针：`downstreamadapter/eventcollector/helper.go:25-31`

- EventCollector：主/redo DynamicStream 入口与路由组件。
 - 代码指针：`downstreamadapter/eventcollector/event_collector.go:176-200`

- DynamicStream：事件分组与调度组件（dynstream）。
 - 代码指针：`downstreamadapter/eventcollector/helper.go:25-45`
