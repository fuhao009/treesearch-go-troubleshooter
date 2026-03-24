---
playbook_id: categraf_performance_radar
scope: coarse-health-escalation
plugin: inputs/performance_radar
---

# Categraf performance_radar 使用手册

## 适用场景

当你希望先做快速分级，再决定往 CPU、内存、文件、网络还是 syscall 方向深入时，用这份手册。

## 步骤 1：先读当前等级和触发器

### 目标

先确定现在是 `L0`、`L1`、`L2`、`L3` 还是 `L4`，以及升级触发条件是什么。

### 关键字段

- `current_level`
- `current_status`
- `trigger`
- `event_time`

### 解释

- `L0`: 正常
- `L1`: 关注
- `L2`: 警告
- `L3`: 严重
- `L4`: 紧急

### 内建采集动作

```tsdiag
{"collector":"host_identity"}
```

```tsdiag
{"collector":"scheduler_overview"}
```

### 本步记录

- 当前等级
- 最近一次升级时间
- 触发子系统

## 步骤 2：读 upgrade_chain 看问题是怎么升级的

### 目标

不要只看当前快照。升级链能告诉你是突然爆发还是逐步恶化。

### 关键字段

- `upgrade_chain[].level`
- `upgrade_chain[].status`
- `upgrade_chain[].upgraded_at`
- `upgrade_chain[].trigger`
- `upgrade_chain[].snapshot`

### 重点判断

- 连续快速升级：更像突发性资源争抢或故障事件
- 长时间停在中等级别：更像慢性劣化
- 触发器始终是同一个子系统：优先沿该子系统深挖

### 本步记录

- 升级链时间线
- 首个异常等级
- 最高等级

## 步骤 3：根据不同等级读取快照粒度

### 目标

知道每个等级的快照关注点，避免在错误层级里找细节。

### L0

只看基础资源概览，例如 CPU、内存、文件、网络、syscall 的基础计数。

### L1

开始看：

- `system_resource`
- `probe_response`
- `system_latency`

### L2 到 L4

逐步包含更多细粒度上下文，例如：

- 子系统平均耗时
- 并发压力
- 错误计数
- 资源消耗

### 本步记录

- 当前快照里最突出的 subsystem
- 平均耗时最高项
- 是否存在多个 subsystem 同时恶化

## 步骤 4：把 performance_radar 映射到主机维度

### 目标

`performance_radar` 是粗粒度预警，不应直接当最终根因。

### 映射建议

- `memory_avg_time` 高：去看内存压力、major fault、swap、OOM
- `file_avg_time` 高：去看磁盘 IO、blocked task、打开文件热点
- `cpu_avg_time` 高：去看 CPU active、load、system CPU、热点进程
- `network_avg_time` 高：去看丢包、重传、timeout、探活失败
- `syscall_avg_time` 高：去看内核态开销、IO 或锁竞争的次生影响

### 内建采集动作

```tsdiag
{"collector":"memory_overview"}
```

```tsdiag
{"collector":"filesystem_overview","limit":10}
```

```tsdiag
{"collector":"network_overview"}
```

### 本步记录

- 预警子系统和主机指标是否一致
- 如果不一致，优先怀疑哪里

## 步骤 5：把 performance_radar 和 evidence_chain 串起来

### 目标

先用 `performance_radar` 定方向，再用 `evidence_chain` 找 actor。

### 推荐顺序

1. 用 `performance_radar` 确定最异常 subsystem
2. 回到主机指标确认是否有同向压力
3. 用 `evidence_chain_process_*` 找出最可能的进程或容器
4. 用日志明细补充命令行、文件热点、端口、容器信息

### 本步记录

- 方向是否收敛
- 哪个进程或容器最值得深挖
- 下一跳去哪个 playbook

## 快速解释模板

- 当前等级：
- 最近触发器：
- 升级路径：
- 对应主机维度：
- 最可疑 actor：
- 还缺的证据：
