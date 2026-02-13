# SyncPoint / Redo 与 Memory Controller 关联分析

**TL;DR**：
- **Too Long; Didn't Read**：本文档聚焦 SyncPoint 与 Memory Controller 的关联交互，不深入 Memory Controller 内部算法
- **核心内容**：SyncPoint 作为阻塞事件（NonBatchable）影响可用内存，进而影响 Redo 的内存配额分配
- **关键结论**：
  - EventCollector 扫描受限时降低 available memory quota
  - Redo 拆分后的配额计算 minMemoryQuota
  - 详见 `docs/code_reading/memorycontroller.md` 了解 Memory Controller 内部机制
- **适用场景**：理解 SyncPoint 如何通过内存机制影响 Redo 行为
- **阅读建议**：先看 `docs/code_reading/memorycontroller.md` 了解基础，再看本文档的关联交互

---

## PR #4030: Memory Controller 优化分析

**PR 标题**: `*:improve memory control`
**状态**: OPEN（未合并）
**作者**: dongmen (asddongmen)

**关键问题**: 当前 memory controller 无法对齐 dispatcher 进度，导致快慢 dispatcher 进度差非常大，在遇到 syncpoint 这种需要对齐进度的 event 时会导致性能严重影响。

### 核心代码变更

#### 1. CongestionControl 版本升级

```golang
// pkg/common/event/congestion_control.go
const (
    CongestionControlVersion1 = 1  // 旧版本
+   CongestionControlVersion2 = 2  // 新版本
)
```

#### 2. EventCollector 新增 changefeed 级别内存跟踪

```golang
// downstreamadapter/eventcollector/event_collector.go
type EventCollector struct {
    // ... 原有字段
+   changefeedUsedMemory map[common.ChangefeedID]uint64  // 新增：跟踪 changefeed 已使用内存
+   changefeedMaxMemory map[common.ChangefeedID]uint64  // 新增：跟踪 changefeed 最大内存
}
```

#### 3. 关键优化：支持 dispatcher 级别内存对齐

**新增方法**：
```golang
// pkg/common/event/congestion_control.go
func (c *CongestionControl) AddAvailableMemoryWithDispatchersAndUsage(
    id common.GID,
    available uint64,
    used uint64,
    max uint64,
    dispatcherAvailable map[common.DispatcherID]uint64  // 新增：dispatcher 级别可用内存
) {
    c.changefeedCount++
    availMem := NewAvailableMemory(id, available)
    availMem.Version = CongestionControlVersion2  // 使用新版本
    availMem.Used = used
    availMem.Max = max
    availMem.DispatcherAvailable = dispatcherAvailable  // 存储 dispatcher 级别可用内存
    availMem.DispatcherCount = uint32(len(dispatcherAvailable))  // dispatcher 数量
    c.availables = append(c.availables, availMem)
}
```

**EventCollector 调用方式**：
```golang
// downstreamadapter/eventcollector/event_collector.go
func (c *EventCollector) newCongestionControlMessages() map[node.ID]*event.Conge {
    // 收集 changefeed 级别的 usedMemory
    changefeedUsedMemory := make(map[common.ChangefeedID]uint64)
    changefeedMaxMemory := make(map[common.ChangefeedID]uint64)

    // 从 main dynamic stream 收集
    for _, quota := range c.ds.GetMetrics().MemoryControl.AreaMemoryMetrics {
        cfID := common.NewChangefeedID(quota.ChangefeedID)
        changefeedUsedMemory[cfID] = uint64(quota.MemoryUsage())
        changefeedMaxMemory[cfID] = uint64(quota.MaxMemory())
    }

    // 从 redo dynamic stream 收集（取最小值）
    for _, quota := range c.redoDs.GetMetrics().MemoryControl.AreaMemoryMetrics {
        cfID := common.NewChangefeedID(quota.ChangefeedID)

        // 关键优化：取最小值避免进度不对齐
        if existing, exists := changefeedUsedMemory[cfID]; exists {
            changefeedUsedMemory[cfID] = min(existing, uint64(quota.MemoryUsage()))
        } else {
            changefeedUsedMemory[cfID] = uint64(quota.MemoryUsage())
        }

        if existing, exists := changefeedMaxMemory[cfID]; exists {
            changefeedMaxMemory[cfID] = min(existing, uint64(quota.MaxMemory()))
        } else {
            changefeedMaxMemory[cfID] = uint64(quota.MaxMemory())
        }
    }

    // 使用优化后的数据构建 congestionControl
    for nodeID, changefeedDispatchers := range nodeDispatcherMemory {
        congestionControl := event.NewCongestionControl()

        // 新方法：传递 dispatcher 级别可用内存
        congestionControl.AddAvailableMemoryWithDispatchersAndUsage(
            changefeedChangefeedID.ID(),
            totalAvailable,
            changefeedUsedMemory[changefeedChangefeedID.ID()],
            changefeedMaxMemory[changefeedChangefeedID.ID()],
            changefeedDispatchers,
        )

        result[nodeID] = congestionControl
    }
}
```

#### 4. 版本兼容性测试

```golang
// pkg/common/event/congestion_control_test.go
func TestCongestionControlMarshalUnmarshalEdgeCases(t *testing.T) {
    // 验证 Version2 的序列化/反序列化
    require.Equal(t, uint16(CongestionControlVersion2), binary.BigEndian.Uint16(data[6:8]), "version")
    require.Equal(t, CongestionControlVersion2, decoded.version)
    require.Equal(t, control.clusterID, decoded.clusterID)
    require.Len(t, decoded.availables, 1)
    require.Equal(t, uint64(3000), decoded.availables[0].Available)
    require.Equal(t, uint64(1200), decoded.availables[0].Used)
    require.Equal(t, uint64(4000), decoded.availables[0].Max)
    require.Equal(t, uint64(2), decoded.availables[0].DispatcherAvailable)  // 新字段
    require.Equal(t, uint32(2), decoded.availables[0].DispatcherCount)      // 新字段
}
```

### 优化核心要点

#### 问题根源
1. **进度不对齐**：快 dispatcher 消费快，慢 dispatcher 消费慢
2. **内存分配不公**：EventCollector 只看 total available memory，不考虑各 dispatcher 进度差异
3. **SyncPoint 放大影响**：需要对齐进度时，慢 dispatcher 被过度限流，导致整体性能下降

#### 解决方案
1. **新增 dispatcher 级别内存跟踪**：
   - `DispatcherAvailable map[common.DispatcherID]uint64` - 每个 dispatcher 的可用内存
   - `DispatcherCount uint32` - dispatcher 数量

2. **取最小值对齐进度**：
   ```go
   changefeedUsedMemory[cfID] = min(existing, uint64(quota.MemoryUsage()))
   changefeedMaxMemory[cfID] = min(existing, uint64(quota.MaxMemory()))
   ```
   - 避免 usedMemory 突然上升
   - 确保 maxMemory 不会过度缩减

3. **版本升级支持向后兼容**：
   - CongestionControlVersion1：旧版本
   - CongestionControlVersion2：新版本（支持 dispatcher 级别信息）

4. **测试验证**：
   - 添加 `TestHandleCongestionControlV2DoesNotAdjustScanInterval` 验证新版本行为
   - 添加 `TestCongestionControlMarshalUnmarshalEdgeCases` 验证序列化兼容性

### 预期效果
1. **更精确的内存控制**：考虑各 dispatcher 实际进度差异
2. **减少快慢差距**：避免慢 dispatcher 被过度限流
3. **SyncPoint 性能提升**：避免因内存配额不对齐导致的过度限流
4. **向后兼容**：通过版本号支持新旧格式共存

---

---

## 现有问题与根本原因

### 问题 1：Dispatcher 进度不对齐

**现象**：快 dispatcher 消费快，慢 dispatcher 消费慢，进度差距非常大。

**根本原因**：EventCollector 的内存控制策略存在缺陷：

1. **全局可用内存视角**：EventCollector 只看 `totalAvailableMemory`，不考虑各 dispatcher 实际进度差异
2. **取最小值对齐的缺陷**：虽然对 main/redo stream 取了最小值，但没有细化到 dispatcher 级别
3. **SyncPoint 阻塞放大影响**：当需要对齐进度时（如 SyncPointEvent），慢 dispatcher 被过度限流，导致整体性能严重下降

```golang
// 现有实现：只看 changefeed 级别的总内存
// downstreamadapter/eventcollector/event_collector.go
totalAvailable := c.changefeedMemoryAvailable[changefeedID]
// ❌ 没有考虑各 dispatcher 的实际进度差异
```

### 问题 2：过度限流导致性能损失

**现象**：慢 dispatcher 因内存压力被过度限制，快 dispatcher 也无法充分发挥性能。

**根本原因**：

1. **配额分配不精确**：CongestionControl 只传递 changefeed 级别的 `usedMemory/maxMemory`
2. **缺少 dispatcher 级别信息**：不知道哪些 dispatcher 快、哪些慢
3. **全或无的控制**：无法针对不同进度 dispatcher 采用差异化策略

### 问题 3：SyncPoint 性能严重影响

**现象**：遇到 SyncPoint 这种需要对齐进度的 event 时，性能严重下降。

**根本原因**：

1. **SyncPoint 是 NonBatchable 事件**：`syncpoint.md:A.6` 说明 SyncPointEvent 被标记为 NonBatchable，需要单独处理
2. **需要进度对齐**：所有 dispatcher 必须到达同步点才能继续
3. **慢 dispatcher 成为瓶颈**：进度差距大时，快 dispatcher 必须等待慢 dispatcher

**影响链路**：
```
SyncPointEvent 生成 (EventService)
  ↓
EventCollector 投递 (DynamicStream NonBatchable)
  ↓
Dispatcher 阻塞处理 (BasicDispatcher)
  ↓
慢 dispatcher 延迟 → 快 dispatcher 等待 → 整体性能下降
```

### PR #4030 优化方案

**核心思路**：从 changefeed 级别内存跟踪升级到 **dispatcher 级别内存跟踪**。

#### 优化 1：Dispatcher 级别可用内存跟踪

```golang
// pkg/common/event/congestion_control.go
type AvailableMemory struct {
    // ... 原有字段
+   DispatcherAvailable map[DispatcherID]uint64  // 新增：每个 dispatcher 的可用内存
+   DispatcherCount     uint32                    // 新增：dispatcher 数量
}
```

#### 优化 2：EventCollector 收集 dispatcher 级别数据

```golang
// downstreamadapter/eventcollector/event_collector.go
func (c *EventCollector) newCongestionControlMessages() {
    // 收集 dispatcher 级别的可用内存
    changefeedDispatchers := make(map[DispatcherID]uint64)

    for _, dispatcherStat := range c.dispatcherStats {
        dispatcherID := dispatcherStat.GetID()
        dispatcherAvailable := dispatcherStat.GetAvailableMemory()
        changefeedDispatchers[dispatcherID] = dispatcherAvailable
    }

    // 使用优化后的方法构建 congestionControl
    congestionControl.AddAvailableMemoryWithDispatchersAndUsage(
        changefeedID,
        totalAvailable,
        changefeedUsedMemory[changefeedID],
        changefeedMaxMemory[changefeedID],
        changefeedDispatchers,  // 新增：dispatcher 级别信息
    )
}
```

#### 优化 3：版本升级支持向后兼容

```golang
// pkg/common/event/congestion_control.go
const (
    CongestionControlVersion1 = 1  // 旧版本
+   CongestionControlVersion2 = 2  // 新版本（支持 dispatcher 级别信息）
)
```

### 预期效果

| 问题 | 优化前 | 优化后 |
|------|--------|--------|
| **进度跟踪粒度** | Changefeed 级别 | Dispatcher 级别 |
| **配额分配** | 不考虑进度差异 | 考虑各 dispatcher 实际进度 |
| **快慢差距** | 差距非常大 | 显著减少 |
| **SyncPoint 性能** | 严重影响 | 性能提升 |
| **向后兼容** | N/A | 通过版本号支持新旧格式共存 |

---

说明：本文档仅聚焦关联点与交互路径；Memory Controller 的内部算法与释放链路详见 `docs/code_reading/memorycontroller.md`。
