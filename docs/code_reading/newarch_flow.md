# NewArch Flow 代码阅读（数据流 + 控制流）

**TL;DR**
- `memorycontroller.md` 已经覆盖了 memory control 策略，但对“谁触发扫描、谁负责调度、控制消息怎么回流”还不够聚焦。
- 本文把 [LogPuller](#c-terminology) -> [EventStore](#c-terminology) -> [EventService](#c-terminology) -> [EventCollector](#c-terminology) -> [Dispatcher](#c-terminology) -> Sink 的数据流和控制流拆开讲清楚。
- 结论先行：
  - 数据写入主链是“上游拉取 + 本地持久化 + 扫描推送 + 下游执行”。
  - 扫描调度主责在 `pkg/eventservice/event_broker.go`，由 EventStore 的 resolved 推进通知驱动，不是盲扫。
  - 控制流分三层：编排层（Maintainer/DispatcherManager）、扫描层（EventService）、内存层（dynstream feedback + congestion control）。

---

目录：
- [A. 范围与核心结论](#sec-a-scope)
- [B. 角色与职责](#sec-b-roles)
- [C. 主数据流（Puller 到 Sink）](#sec-c-dataflow)
  - [C.1 TiKV 到 EventStore](#sec-c1-tikv-to-store)
  - [C.2 EventStore 到 EventService 扫描入口](#sec-c2-store-to-broker)
  - [C.3 EventService 到 EventCollector 到 Sink](#sec-c3-broker-to-sink)
- [D. 主控制流（谁在调度）](#sec-d-controlflow)
  - [D.1 编排控制：谁创建/删除 Dispatcher](#sec-d1-orchestration)
  - [D.2 扫描控制：谁触发 scan task](#sec-d2-scan-schedule)
  - [D.3 流控控制：Pause/Release/Quota](#sec-d3-flowcontrol)
- [E. EventService 扫描细化（重点）](#sec-e-scan-detail)
- [F. 可靠性与降级](#sec-f-reliability)
- [G. 配置项与可观测性](#sec-g-config-observe)
- [H. 术语汇总小节](#c-terminology)

---

<a id="sec-a-scope"></a>
## A. 范围与核心结论

**summary**
- 覆盖范围：
  - 聚焦新架构里“表事件从上游进入直到下游写出”的主路径，以及扫描/调度/回压控制路径。
  - 重点补充 [EventService](#c-terminology) 的扫描触发与调度细节（这是 `memorycontroller.md` 里相对轻描的部分）。
- 非目标：
  - 不展开 SQL 编解码细节、不展开 Maintainer 全量调度算法细节（只保留与事件流直接相关的控制面接口）。

**核心结论**
- 不是 EventCollector 主动去扫 EventStore；是 EventService 的 broker 收到 EventStore 的 resolved 推进通知后，再调度 scan task。
- 调度不只一条链：
  - 编排调度：Maintainer -> DispatcherManager -> EventCollector/EventService（创建/删除/迁移 Dispatcher）。
  - 扫描调度：EventService.eventBroker（scan task 触发、排队、限速、配额）。
  - 内存调度：dynstream（PauseArea/ResumeArea/ReleasePath）+ EventCollector 的 CongestionControl 上报。

---

<a id="sec-b-roles"></a>
## B. 角色与职责

**summary**
- 主链路角色分工：
  - [LogPuller](#c-terminology) 负责从 TiKV 拉取 region 变更并按 subscription 入队。
  - [EventStore](#c-terminology) 负责持久化与可重扫窗口。
  - [EventService](#c-terminology) 负责扫描调度与向 EventCollector 发送事件。
  - [EventCollector](#c-terminology) 负责接收/校验/路由到 [Dispatcher](#c-terminology)。
  - Dispatcher + Sink 负责最终下游写入与 checkpoint 推进。

| 组件 | 关键职责 | 关键代码 |
|---|---|---|
| SubscriptionClient / regionRequestWorker | 拉取 TiKV 事件，分发到 dynstream path | `logservice/logpuller/subscription_client.go`, `logservice/logpuller/region_request_worker.go` |
| regionEventHandler | 解析 region event，调用 consumeKVEvents，回调 wake | `logservice/logpuller/region_event_handler.go` |
| EventStore | 写 Pebble、管理订阅复用、对外提供 iterator | `logservice/eventstore/event_store.go` |
| EventService(eventBroker) | 管理 dispatcherStat、调度 scan task、发送消息 | `pkg/eventservice/event_service.go`, `pkg/eventservice/event_broker.go` |
| EventScanner | merge DML/DDL/resolved，按限制中断扫描 | `pkg/eventservice/event_scanner.go` |
| EventCollector | 收消息推 ds、处理 feedback、发送 congestion control | `downstreamadapter/eventcollector/event_collector.go` |
| dispatcherStat(EventCollector) | 校验 epoch/seq，drop/reset，ready/handshake 状态机 | `downstreamadapter/eventcollector/dispatcher_stat.go` |
| EventDispatcher/BasicDispatcher | 执行事件到 sink，推进 resolved/checkpoint | `downstreamadapter/dispatcher/event_dispatcher.go`, `downstreamadapter/dispatcher/basic_dispatcher.go` |

---

<a id="sec-c-dataflow"></a>
## C. 主数据流（Puller 到 Sink）

**summary**
- 主数据流是“先持久化，再扫描推送”：
  - 先由 [LogPuller](#c-terminology) 把 KV 送入 [EventStore](#c-terminology)。
  - 再由 [EventService](#c-terminology) 基于 resolved 进度扫描 EventStore 推送下游。
- 这样做的直接效果：
  - EventCollector 发生 ReleasePath 或消息丢弃时，可通过 reset 从 EventStore 重拉，避免永久丢失。

**跨模块时序图（主路径）**

```text
TiKV
  -> regionRequestWorker.recv()
  -> subscriptionClient.pushRegionEventToDS()
  -> logpuller dynstream(regionEventHandler)
  -> EventStore.consumeKVEvents() -> eventCh.Push()
  -> EventStore.writeWorker.writeEvents(Pebble) -> callback(wake)
  -> EventStore notifier(resolved, maxCommitTs)
  -> EventService.eventBroker.onNotify()
  -> pushTask(scanWorker) -> doScan() -> eventScanner.scan()
  -> runSendMessageWorker() -> MessageCenter(EventCollectorTopic)
  -> EventCollector.runDispatchMessage() -> ds.Push(dispatcherID)
  -> dispatcherStat.handleDataEvents()
  -> Dispatcher.HandleEvents() -> sink
```

<a id="sec-c1-tikv-to-store"></a>
### C.1 TiKV 到 EventStore

**summary**
- 这一段是“上游拉取 + 入队 + 持久化”。
- 关键控制点：
  - `pushRegionEventToDS` 会受 paused 控制（上游 PauseArea/ResumeArea）。
  - KV 真正从 pendingQueue 释放，要等 EventStore 写入完成后的 callback。

**调用链（缩进）**
- 收取 TiKV gRPC 事件
  - `regionRequestWorker.receiveAndDispatchChangeEvents`
    - `dispatchRegionChangeEvents / dispatchResolvedTsEvent`
      - `subscriptionClient.pushRegionEventToDS`
- dynstream 处理 path 事件
  - `regionEventHandler.Handle`
    - `span.consumeKVEvents(kvs, finishCallback)`
      - `eventStore.consumeKVEvents`（闭包）
        - `subStat.eventCh.Push(eventWithCallback)`
- EventStore 持久化
  - `writeTaskPool.run`
    - `writeEvents(db, events)`
    - `events[idx].callback()`（回调 wake + 释放 pending）

```go
// logservice/logpuller/subscription_client.go:399-412
func (s *subscriptionClient) pushRegionEventToDS(subID SubscriptionID, event regionEvent) {
    if !s.paused.Load() {
        s.ds.Push(subID, event)
        return
    }
    s.mu.Lock()
    for s.paused.Load() {
        s.cond.Wait()
    }
    s.mu.Unlock()
    s.ds.Push(subID, event)
}
```

```go
// logservice/eventstore/event_store.go:603-611
subStat.eventCh.Push(eventWithCallback{
    subID: subStat.subID,
    kvs:   kvs,
    callback: finishCallback,
})
return true
```

```go
// logservice/eventstore/event_store.go:333-350
events, ok := p.dataCh.GetMultipleNoGroup(buffer)
if err = p.store.writeEvents(p.db, events, encoder); err != nil { ... }
for idx := range events {
    events[idx].callback()
}
```

<a id="sec-c2-store-to-broker"></a>
### C.2 EventStore 到 EventService 扫描入口

**summary**
- EventService 不是自己定时全表扫；普通表扫描由 EventStore resolved 推进通知触发。
- 触发链：EventStore `advanceResolvedTs` -> notifier -> `eventBroker.onNotify` -> `scanReady` -> `pushTask`。

**调用链（缩进）**
- Dispatcher 注册到 EventService
  - `eventBroker.addDispatcher`
    - `eventStore.RegisterDispatcher(... notifier ...)`
- EventStore resolved 推进
  - `advanceResolvedTs(ts)` 触发 subscriber notify
    - notifier 回调 `eventBroker.onNotify(d, resolvedTs, latestCommitTs)`
      - `scanReady(d)` 判断是否需要扫描
      - `pushTask(d, true)` 投递 scan task

```go
// pkg/eventservice/event_broker.go:945-957
success := c.eventStore.RegisterDispatcher(..., func(resolvedTs uint64, latestCommitTs uint64) {
    d := dispatcherPtr.Load()
    if d.isRemoved.Load() {
        return
    }
    c.onNotify(d, resolvedTs, latestCommitTs)
}, ...)
```

```go
// pkg/eventservice/event_broker.go:878-885
func (c *eventBroker) onNotify(d *dispatcherStat, resolvedTs uint64, commitTs uint64) {
    if d.onResolvedTs(resolvedTs) {
        d.onLatestCommitTs(commitTs)
        if c.scanReady(d) {
            c.pushTask(d, true)
        }
    }
}
```

<a id="sec-c3-broker-to-sink"></a>
### C.3 EventService 到 EventCollector 到 Sink

**summary**
- EventService 把 scan 结果封装后通过 MessageCenter 推给 EventCollector。
- EventCollector 只负责接收/路由/校验，最终由 Dispatcher 写 Sink。

**调用链（缩进）**
- EventService 扫描并发送
  - `runScanWorker` -> `doScan`
    - `eventScanner.scan`
    - `sendDML/sendDDL/sendResolvedTs`
  - `runSendMessageWorker` -> `sendMsg(SendEvent)`
- EventCollector 接收并路由
  - `MessageCenterHandler` -> `runDispatchMessage`
    - `ds.Push(dispatcherID, dispatcherEvent)`
  - `EventsHandler.Handle`
    - `dispatcherStat.handleDataEvents`
      - `target.HandleEvents(events, wakeCallback)`
- Dispatcher 执行
  - `EventDispatcher.HandleEvents` -> `BasicDispatcher.handleEvents`
    - DML: `AddDMLEventsToSink`
    - DDL/SyncPoint: `DealWithBlockEvent`

```go
// downstreamadapter/eventcollector/event_collector.go:541-566
func (c *EventCollector) runDispatchMessage(ctx context.Context, inCh <-chan *messaging.TargetMessage, mode int64) error {
    ...
    case e := msg.(event.Event):
        dispatcherEvent := dispatcher.NewDispatcherEvent(&targetMessage.From, e)
        ds.Push(e.GetDispatcherID(), dispatcherEvent)
}
```

```go
// downstreamadapter/eventcollector/helper.go:72-86
func (h *EventsHandler) Handle(stat *dispatcherStat, events ...dispatcher.DispatcherEvent) bool {
    switch events[0].GetType() {
    case commonEvent.TypeDMLEvent, commonEvent.TypeResolvedEvent, commonEvent.TypeDDLEvent, ...:
        return stat.handleDataEvents(events...)
    ...
    }
}
```

---

<a id="sec-d-controlflow"></a>
## D. 主控制流（谁在调度）

**summary**
- 调度职责拆分：
  - 编排调度：Maintainer/DispatcherManager 负责“有哪些 Dispatcher”。
  - 扫描调度：EventService.eventBroker 负责“何时扫描、扫多少、发多少”。
  - 流控调度：dynstream + congestion control 负责“何时停/丢/降速”。

<a id="sec-d1-orchestration"></a>
### D.1 编排控制：谁创建/删除 Dispatcher

**summary**
- 外层调度不是 EventService 决定，而是 Maintainer 驱动 DispatcherManager。
- EventCollector/EventService 主要执行“注册/重置/移除”请求。

**调用链（缩进）**
- Maintainer 侧发 bootstrap/schedule 请求
  - `DispatcherOrchestrator.RecvMaintainerRequest` 入队
    - `handleBootstrapRequest` 创建 DispatcherManager
- DispatcherManager 创建 dispatcher
  - `eventCollector.AddDispatcher(dispatcher, memoryQuota)`
    - `dispatcherStat.registerTo(eventService)` 发送 register request
- EventService 收到 request
  - `eventService.handleMessage -> registerDispatcher/resetDispatcher/removeDispatcher`

```go
// downstreamadapter/dispatcherorchestrator/dispatcher_orchestrator.go:130-138
switch req := msg.Message[0].(type) {
case *heartbeatpb.MaintainerBootstrapRequest:
    _ = m.handleBootstrapRequest(msg.From, req)
...
}
```

```go
// pkg/eventservice/event_service.go:119-127
case info := <-s.dispatcherInfoChan:
    switch info.GetActionType() {
    case REGISTER: s.registerDispatcher(ctx, info)
    case REMOVE:   s.deregisterDispatcher(info)
    case RESET:    s.resetDispatcher(info)
    }
```

<a id="sec-d2-scan-schedule"></a>
### D.2 扫描控制：谁触发 scan task

**summary**
- 普通表扫描触发是“事件驱动”，不是固定周期扫全量。
- 关键调度器就是 `eventBroker.pushTask` + `taskChan[scanWorkerIndex]`。

**关键机制**
- 每个 dispatcher 绑定一个 `scanWorkerIndex`（hash 后固定 worker），保证单 path 串行扫描。
- `isTaskScanning` CAS 保证同一 dispatcher 同时最多一个在跑。
- table trigger dispatcher 额外有 `tickTableTriggerDispatchers`（50ms）路径，用于 DDL span 周期检查。

```go
// pkg/eventservice/dispatcher_stat.go:145-146
scanWorkerIndex:    (common.GID)(id).Hash(scanWorkerCount),
messageWorkerIndex: (common.GID)(id).Hash(messageWorkerCount),
```

```go
// pkg/eventservice/event_broker.go:894-901
if !d.isTaskScanning.CompareAndSwap(false, true) {
    return
}
c.taskChan[d.scanWorkerIndex] <- d
```

```go
// pkg/eventservice/event_broker.go:310-318
ticker := time.NewTicker(50 * time.Millisecond)
case <-ticker.C:
    c.tableTriggerDispatchers.Range(func(...) bool { ... })
```

<a id="sec-d3-flowcontrol"></a>
### D.3 流控控制：Pause/Release/Quota

**summary**
- 流控也分层，不是单点控制：
  - 上游用 PauseArea/ResumeArea 阻塞拉取（宁可慢，不丢上游源数据）。
  - 下游用 ReleasePath 释放 path 队列（允许丢临时缓存，再 reset 重拉）。
  - EventCollector 每秒回报 available memory 给 EventService，EventService 据此降速/跳扫。

**上游 Pause/Resume（SubscriptionClient）**
```go
// logservice/logpuller/subscription_client.go:420-427
case dynstream.PauseArea:
    s.paused.Store(true)
case dynstream.ResumeArea:
    s.paused.Store(false)
    s.cond.Broadcast()
```

**下游 ReleasePath（EventCollector）**
```go
// downstreamadapter/eventcollector/event_collector.go:422-431
case feedback := <-c.ds.Feedback():
    if feedback.FeedbackType == dynstream.ReleasePath {
        c.ds.Release(feedback.Path)
    }
```

**CongestionControl（EventCollector -> EventService）**
```go
// downstreamadapter/eventcollector/event_collector.go:577-590
ticker := time.NewTicker(time.Second)
messages := c.newCongestionControlMessages()
msg := messaging.NewSingleTargetMessage(serverID, messaging.EventServiceTopic, m)
_ = c.mc.SendCommand(msg)
```

```go
// pkg/eventservice/event_broker.go:606-619
if available.Load() < c.scanLimitInBytes { task.resetScanLimit() }
if !allocQuota(available, uint64(sl.maxDMLBytes)) {
    c.sendSignalResolvedTs(task)
    metrics.EventServiceSkipScanCount.WithLabelValues("changefeed_quota").Inc()
    return
}
```

---

<a id="sec-e-scan-detail"></a>
## E. EventService 扫描细化（重点）

**summary**
- 这一节回答“EventService 的扫是谁在控、怎么控、何时不扫”。
- 结论：`eventBroker.doScan` 是扫描执行核心，前后有完整的前置检查、配额扣减、限速、可中断重排队。

**1) 扫描前置状态**
- `scanReady` 先做硬条件过滤：
  - dispatcher 未删除；
  - 当前无在途 scan task；
  - ready/handshake 检查通过。
- `getScanTaskDataRange` 再做数据条件过滤：
  - 从 `dispatcherStat.getDataRange()` 得到 `(lastScanned, receivedResolved]`；
  - 用 schemaStore DDL state 收紧 `CommitTsEnd`；
  - 若确认没新 DML/DDL，只发 resolved，不扫 store。

```go
// pkg/eventservice/event_broker.go:436-453
if task.isRemoved.Load() || task.isTaskScanning.Load() { return false }
if !c.checkAndSendReady(task) { return false }
ok, _ := c.getScanTaskDataRange(task)
return ok
```

**2) 扫描调度与并发模型**
- `pushTask` 使用 `isTaskScanning` 防并发。
- worker 固定：`taskChan[scanWorkerIndex]`，由 `runScanWorker` 顺序执行 `doScan`。
- 中断重试：`doScan` defer 中若 `interrupted=true`，`pushTask(task, false)` 重新排队。

```go
// pkg/eventservice/event_broker.go:543-549
defer func() {
    task.isTaskScanning.Store(false)
    if interrupted {
        c.pushTask(task, false)
    }
}()
```

**3) doScan 的控制门**
- 目标可达门：`msgSender.IsReadyToSend(remoteID)`。
- 速率门：`scanRateLimiter.AllowN(now, lastScanBytes)`。
- 配额门：
  - changefeed 级 available memory；
  - dispatcher 级 available memory。
- 通过后才进入 `eventScanner.scan`。

**4) 扫描执行（eventScanner）**
- 先取 DDL：`FetchTableDDLEvents(start,end)`。
- 再扫 DML iterator：`eventStore.GetIterator(dispatcherID, dataRange)`。
- merge 规则：按 commitTs 顺序输出；同 commitTs 下 DML 在 DDL 前。
- 中断规则：只在 commitTs 边界允许中断，保证语义连续性。

```go
// pkg/eventservice/event_scanner.go:226-229
if session.exceedLimit(...) && merger.canInterrupt(rawEvent.CRTs, processor.batchDML) {
    interruptScan(session, merger, processor, rawEvent.CRTs, rawEvent.StartTs)
    return true, nil
}
```

**5) 发送与失败处理**
- 扫描结果进入 message worker，resolved 先做 cache/批量。
- `sendMsg` 对 `"congested"` 错误会 sleep+重试；其他发送错误会 drop，依赖下游序列检查触发 reset 后重拉。

```go
// pkg/eventservice/event_broker.go:816-820
log.Info("send message failed, drop it", ...)
// If the dispatcher finds events are not continuous, it will send a reset message.
return
```

---

<a id="sec-f-reliability"></a>
## F. 可靠性与降级

**summary**
- 核心可靠性依赖“可重扫 + 序列校验 + reset 补偿”，不是每跳强 ACK。
- 关键前提是 EventStore 中目标区间仍未被 GC。

**关键点**
- EventCollector 发现 out-of-order/drop 会 reset dispatcher：
  - 序列不连续：`verifyEventSequence` 返回 false 后触发 `reset(...)`。
  - 收到 DropEvent：`handleDropEvent` 直接 reset。
- reset 后 EventService 用更大 epoch 重新发送 handshake + 后续数据，旧 epoch 数据在 EventCollector 被丢弃。
- checkpoint 推进后 EventStore 执行 GC；一旦区间被 GC，reset 重拉窗口就会缩小。

```go
// downstreamadapter/eventcollector/dispatcher_stat.go:399-401
if !d.verifyEventSequence(event) {
    d.reset(d.connState.getEventServiceID())
    return false
}
```

```go
// downstreamadapter/eventcollector/dispatcher_stat.go:604-611
log.Info("received a dropEvent, need to reset the dispatcher", ...)
d.reset(d.connState.getEventServiceID())
```

---

<a id="sec-g-config-observe"></a>
## G. 配置项与可观测性

**summary**
- 本链路里既有可配置项，也有硬编码默认值。
- 排障时建议先看 memory + skip scan + reset 相关指标与日志。

### 配置项（默认/入口/生效点）

| 配置项 | 默认 | 配置入口 | 生效代码 |
|---|---:|---|---|
| changefeed memory quota | 1GB | replica config: `memory-quota` | `pkg/config/server.go:44-46`, `pkg/config/replica_config.go:396-399`, `downstreamadapter/dispatchermanager/dispatcher_manager.go:199` |
| EventService scan task queue | 8192 | server debug: `debug.event-service.scan-task-queue-size` | `pkg/config/debug.go:115-136`, `pkg/eventservice/event_broker.go:115-116` |
| EventService scan limit | 256MB | server debug: `debug.event-service.scan-limit-in-bytes` | `pkg/config/debug.go:117-137`, `pkg/eventservice/event_broker.go:118-143` |
| Puller dynstream area quota | 1GB（当前硬编码） | 无独立配置 | `logservice/logpuller/subscription_client.go:364-365` |
| EventCollector path/area quota | 来自 changefeed quota | DispatcherManager -> EventCollector.AddDispatcher | `downstreamadapter/dispatchermanager/task.go:187-207`, `downstreamadapter/eventcollector/event_collector.go:270-271` |

### 可观测性（建议优先看）

- 上游拉取与内存：
  - `DynamicStreamEventChanSize{module="event-store"}` / `DynamicStreamPendingQueueLen{module="event-store"}`
  - 日志：`subscription client pause push region event` / `resume ...`
- EventService 扫描：
  - `EventServiceSkipScanCount{changefeed_quota|dispatcher_quota}`
  - `EventServiceInterruptScanCount`
  - `EventServiceAvailableMemoryQuotaGaugeVec`
- EventStore：
  - `EventStoreWriteQueueDurationHistogram`, `EventStoreResolvedTsLagGauge`
  - 日志：`resolved ts lag is too large for initialized subscription`
- EventCollector/Dispatcher：
  - `DynamicStreamMemoryUsage{module="event-collector"}`
  - `EventCollectorDroppedEventCount`
  - reset/out-of-order 相关 warn 日志

---

<a id="c-terminology"></a>
## H. 术语汇总小节

**summary**
- 本节给出本文关键术语的统一定义和代码指针。
- 正文中的术语都可回跳到这里对照。

- **[LogPuller](#c-terminology)**：
  - 定义：从 TiKV 拉取 region CDC 事件并送入 dynstream 的上游入口。
  - 代码：`logservice/logpuller/subscription_client.go`, `logservice/logpuller/region_request_worker.go`。

- **[EventStore](#c-terminology)**：
  - 定义：本地 Pebble 持久化层；承接上游写入并提供按时间范围扫描。
  - 代码：`logservice/eventstore/event_store.go`。

- **[EventService](#c-terminology)**：
  - 定义：服务层；管理 dispatcher、调度扫描、把事件发送给 EventCollector。
  - 代码：`pkg/eventservice/event_service.go`, `pkg/eventservice/event_broker.go`。

- **[eventBroker](#c-terminology)**：
  - 定义：EventService 内核心调度器；维护 dispatcherStat、scan worker、send worker。
  - 代码：`pkg/eventservice/event_broker.go`。

- **[EventCollector](#c-terminology)**：
  - 定义：EventService 与 Dispatcher 之间的中继；负责消息路由、ds memory control、congestion control 回传。
  - 代码：`downstreamadapter/eventcollector/event_collector.go`。

- **[Dispatcher](#c-terminology)**：
  - 定义：表级执行单元；处理 DML/DDL/SyncPoint 并写入 sink，维护 checkpoint/resolved。
  - 代码：`downstreamadapter/dispatcher/basic_dispatcher.go`, `downstreamadapter/dispatcher/event_dispatcher.go`。

- **[scan task](#c-terminology)**：
  - 定义：EventService 针对单 dispatcher 的一次扫描任务（由 resolved 推进触发）。
  - 代码：`pkg/eventservice/event_broker.go:297-305`, `pkg/eventservice/event_broker.go:889-909`。

- **[CongestionControl](#c-terminology)**：
  - 定义：EventCollector 周期上报 available memory，EventService 据此控制扫描量/跳扫。
  - 代码：`downstreamadapter/eventcollector/event_collector.go:577-706`, `pkg/eventservice/event_broker.go:1184-1218`。

- **[ReleasePath](#c-terminology)**：
  - 定义：EventCollector dynstream 在压力下释放 path 队列的反馈动作。
  - 代码：`downstreamadapter/eventcollector/event_collector.go:415-433`, `utils/dynstream/memory_control.go:174-217`。

- **[PauseArea/ResumeArea](#c-terminology)**：
  - 定义：Puller dynstream 的 area 级暂停/恢复反馈，用于阻塞上游 push。
  - 代码：`utils/dynstream/memory_control.go:225-276`, `logservice/logpuller/subscription_client.go:420-427`。

