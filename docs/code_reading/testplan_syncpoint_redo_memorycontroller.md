# SyncPoint / Redo / Memory Controller 测试方案草案（代码验证版）

说明：该测试方案用于支撑“syncpoint + Memory Controller（见[术语汇总小节](#c-terminology)）性能劣化”与“redo 可用性/性能未知”的调研；方案内容遵循 `docs/code_reading/writingstandard.md`。

目录：
- [A. 测试目标与范围](#sec-a-goal)
- [B. 测试矩阵与变量](#sec-b-matrix)
- [C. 观测指标与采集位置](#sec-c-metrics)
- [D. 测试场景设计](#sec-d-cases)
  - [D.1 SyncPoint 正确性（功能）](#sec-d1-functional)
  - [D.2 性能与吞吐（lag/scan/flush）](#sec-d2-performance)
  - [D.3 稳定性长跑（内存/lag）](#sec-d3-stability)
  - [D.4 故障与恢复（重启/重置）](#sec-d4-recovery)
- [E. 环境与数据集](#sec-e-env)
- [F. 通过/失败判定](#sec-f-passfail)
- [G. 风险与前置条件](#sec-g-risk)
- [H. 可配置项说明](#sec-h-config)
- [I. 术语汇总小节](#c-terminology)

<a id="sec-a-goal"></a>
## A. 测试目标与范围

summary：
- 目标：验证 syncpoint + Memory Controller（见[术语汇总小节](#c-terminology)）是否导致 lag 持续上升
  - 同步评估 redo 启用后的性能与稳定性

范围：
- 新架构（EventCollector（见[术语汇总小节](#c-terminology)） + DynamicStream（见[术语汇总小节](#c-terminology)） + EventService（见[术语汇总小节](#c-terminology)））链路
- 下游为 MySQL/TiDB（syncpoint 生效前提）
- redo 启用/关闭对比

涉及组件：Dispatcher（见[术语汇总小节](#c-terminology)）与 Sink（见[术语汇总小节](#c-terminology)）。

时序图（简化）：
```
EventService --(events)--> EventCollector --(DynamicStream)--> Dispatcher --(syncpoint/redo)--> Sink
```

<a id="sec-b-matrix"></a>
## B. 测试矩阵与变量

summary：
- 变量按“功能开关 + 资源配额 + 负载模式”分层组合
  - 先用小矩阵筛选，再扩展组合覆盖

建议矩阵（草案）：
- 功能开关：
  - syncpoint：{off, on}
  - redo：{off, on}
- 资源配额：
  - MemoryQuota（见[术语汇总小节](#c-terminology)）：{小(默认), 中, 大}
  - ConsistentConfig（见[术语汇总小节](#c-terminology)）.MemoryUsage.MemoryQuotaPercentage：{25, 50(默认), 75}
- 负载模式：
  - DML-only（小事务 / 大事务）
  - DML + DDL 混合（含多表 DDL）

代码参考：
- memory quota 拆分：`downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:53-60`
- syncpoint 生成：`pkg/eventservice/event_broker.go:518-534`

<a id="sec-c-metrics"></a>
## C. 观测指标与采集位置

summary：
- 优先观测：DynamicStream（见[术语汇总小节](#c-terminology)）内存、EventService（见[术语汇总小节](#c-terminology)） scan 跳过、redo 进度
  - 用于定位 Memory Controller 触发与 lag 变化的因果链路

指标与代码定位：
- DynamicStream memory usage：
  - `metrics.DynamicStreamMemoryUsage` 由 EventCollector（见[术语汇总小节](#c-terminology)）维护
  - 代码指针：`downstreamadapter/eventcollector/event_collector.go:78-95`

- EventService skip scan 计数：
  - `EventServiceSkipScanCount` 在 quota 不足时累加
  - 代码指针：`pkg/eventservice/event_broker.go:612-627`

- redo resolvedTs 进度：
  - RedoMeta（见[术语汇总小节](#c-terminology)）驱动的 RedoResolvedTsProgressMessage 上报
  - 代码指针：`downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:285-311`

- 主链路 redoGlobalTs（见[术语汇总小节](#c-terminology)）缓存触发：
  - `EventDispatcher.cache` 分支
  - 代码指针：`downstreamadapter/dispatcher/event_dispatcher.go:135-142`

- syncpoint 写入成功率与延迟：
  - FlushSyncPointEvent 路径
  - 代码指针：`pkg/sink/mysql/mysql_writer.go:169-203`

<a id="sec-d-cases"></a>
## D. 测试场景设计

summary：
- 按“功能正确性 -> 性能 -> 稳定性 -> 故障恢复”递进
  - 每一阶段都需要记录 lag / memory / redo 进度

<a id="sec-d1-functional"></a>
### D.1 SyncPoint 正确性（功能）

summary：
- 验证 syncpoint_v1 是否持续写入，并与 ddl_ts_v1 记录一致
  - 检查 syncpoint 与 DDL 的 commitTs 边界行为

校验点：
- syncpoint 表是否存在并持续插入
  - 代码指针：`pkg/sink/mysql/mysql_writer_for_syncpoint.go:31-131`
- ddl_ts_v1 syncpoint meta row 是否更新
  - 代码指针：`pkg/sink/mysql/mysql_writer_for_ddl_ts.go:30-147`

<a id="sec-d2-performance"></a>
### D.2 性能与吞吐（lag/scan/flush）

summary：
- 对比不同 SyncPointInterval 与 MemoryQuota
  - 关注 scan 跳过率与 lag 曲线趋势

建议观测：
- lag 与 skip-scan 是否同步上升
  - 代码指针：`pkg/eventservice/event_broker.go:612-627`
- DynamicStream memory usage 是否持续增长
  - 代码指针：`downstreamadapter/eventcollector/event_collector.go:78-95`

<a id="sec-d3-stability"></a>
### D.3 稳定性长跑（内存/lag）

summary：
- 长时间运行（>= 12h）验证 lag 是否持续上升
  - redo 开关两组对比

关注点：
- redoGlobalTs 追赶是否稳定
  - 代码指针：`downstreamadapter/dispatcher/event_dispatcher.go:135-142`
- redo meta 是否持续上报
  - 代码指针：`downstreamadapter/dispatchermanager/dispatcher_manager_redo.go:285-311`

<a id="sec-d4-recovery"></a>
### D.4 故障与恢复（重启/重置）

summary：
- 验证重启后 syncpoint/redo 的恢复语义
  - startTs 与 syncpoint meta row 对齐

关注点：
- SyncPointTs 计算逻辑与 skipSyncpointAtStartTs
  - 代码指针：`downstreamadapter/syncpoint/sync_point.go:22-38`
- 重启后的 ddl_ts_v1 / syncpoint_v1 行为
  - 代码指针：`pkg/sink/mysql/mysql_writer_for_ddl_ts.go:30-147`

<a id="sec-e-env"></a>
## E. 环境与数据集

summary：
- 环境需能稳定复现实验结果
  - 建议固定硬件规格与负载生成方式

建议：
- TiDB + TiCDC + 下游 TiDB/MySQL
- 负载工具：可复用现有 DML/DDL 压测脚本（按实际环境选择）
- 数据集：小/中/大表混合（含热点表）

<a id="sec-f-passfail"></a>
## F. 通过/失败判定

summary：
- 判定以“lag 曲线 + memory 控制行为 + redo 进度”综合判断
  - 需要记录对照组（syncpoint off / redo off）

示例判定（草案）：
- 通过：lag 在稳定区间内波动，skip-scan 可解释且不持续上升
- 失败：lag 持续单调上升，或 redoGlobalTs 长期停滞

<a id="sec-g-risk"></a>
## G. 风险与前置条件

summary：
- 当前调研阻塞条件需先解决
  - Memory Controller + syncpoint 组合的性能问题未解决前，redo 性能测试结果可能不稳定

前置条件：
- 建议先收敛 syncpoint + Memory Controller 的性能劣化
- redo 性能测试需在稳定基线后进行

<a id="sec-h-config"></a>
## H. 可配置项说明

summary：
- 可配置项决定测试矩阵与对照组
  - 通过 changefeed config 文件（`--config`）下发

<a id="sec-h1-defaults"></a>
### H.1 默认值

summary：
- 给出测试基线默认值
  - 作为对照组的起点

```golang
// pkg/config/replica_config.go:46-53
MemoryQuota:        util.AddressOf(uint64(DefaultChangefeedMemoryQuota)),
EnableSyncPoint:    util.AddressOf(false),
SyncPointInterval:  util.AddressOf(10 * time.Minute),
SyncPointRetention: util.AddressOf(24 * time.Hour),
```

```golang
// pkg/config/server.go:44-45
DefaultChangefeedMemoryQuota = 1024 * 1024 * 1024 // 1GB.
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

<a id="sec-h2-configs"></a>
### H.2 关键配置与入口

summary：
- 关键配置项及其生效点
  - 影响 syncpoint/redo/Memory Controller 行为

关键配置：
- SyncPointInterval / SyncPointRetention / EnableSyncPoint
  - 代码指针：`pkg/config/replica_config.go:149-163`
- ConsistentConfig（见[术语汇总小节](#c-terminology)）.Level / MemoryUsage
  - 代码指针：`pkg/config/consistent.go:26-72`
- MemoryQuota（见[术语汇总小节](#c-terminology)）
  - 代码指针：`pkg/config/replica_config.go:46-53`

```golang
// cmd/cdc/cli/cli_changefeed_create.go:60-65
cmd.PersistentFlags().StringVar(&o.configFile, "config", "", "Path of the configuration file")
```

<a id="c-terminology"></a>
## I. 术语汇总小节

summary：
- 关键术语与代码指针
  - 每个术语给出定义与定位点

- DynamicStream：事件分组与调度组件（dynstream）。
  - 代码指针：`downstreamadapter/eventcollector/helper.go:25-45`

- EventCollector：事件中转与分发组件。
  - 代码指针：`downstreamadapter/eventcollector/event_collector.go:105-129`

- EventService：事件扫描与发送组件。
  - 代码指针：`pkg/eventservice/event_broker.go:588-627`

- Memory Controller：DynamicStream 内部的内存控制机制。
  - 代码指针：`downstreamadapter/eventcollector/helper.go:25-31`

- SyncPointEvent：阻塞事件类型。
  - 代码指针：`downstreamadapter/dispatcher/basic_dispatcher.go:679-697`

- Dispatcher：下游事件处理单元。
  - 代码指针：`downstreamadapter/dispatcher/basic_dispatcher.go:540-725`

- Sink：下游写入抽象（含 MySQL/TiDB sink）。
  - 代码指针：`downstreamadapter/sink/mysql/sink.go:269-305`

- RedoMeta：redo 进度元数据。
  - 代码指针：`downstreamadapter/sink/redo/meta.go:24-120`

- redoGlobalTs：主链路前进门槛。
  - 代码指针：`downstreamadapter/dispatcher/event_dispatcher.go:135-142`

- ConsistentConfig：redo 功能配置入口。
  - 代码指针：`pkg/config/consistent.go:26-72`

- MemoryQuota：changefeed 内存配额配置字段。
  - 代码指针：`pkg/config/replica_config.go:46-53`
