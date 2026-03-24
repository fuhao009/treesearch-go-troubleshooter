---
playbook_id: host_anomaly
scope: linux-host
references:
  - vm-host-anomaly-analysis
  - categraf evidence_chain
  - categraf performance_radar
---

# Linux 主机异常排查总流程

## 适用场景

适用于以下故障：

- 宿主机 CPU、内存、IO、网络异常
- 容器或服务重启
- 指标黑屏、探活失败、性能突降
- 需要从“症状”逐步收敛到“最可能根因”的场景

## 步骤 1：锁定时间窗口与影响面

### 目标

先确认问题发生在什么绝对时间、哪个时区、影响到了哪些服务和机器。不要直接用“刚刚”“今天上午”这种相对描述。

### 要收集的输入

- `agent_hostname`
- 开始时间、结束时间、时区
- 受影响服务名、端口、探活地址
- 用户感知影响：超时、错误率、重启、抖动、吞吐下降

### 内建采集动作或接口

```tsdiag
{"collector":"host_identity"}
```

```text
GET /api/v1/series?match[]={agent_hostname="..."}&start=...&end=...
```

### 本步记录

- 统一写成绝对时间，例如 `2026-03-24 09:12:00 +08:00`
- 记录受影响对象和未受影响对象
- 如果窗口还不清楚，先拉大范围，再缩小到故障边界

## 步骤 2：确认观测面是否完整

### 目标

不要把“指标消失”直接当成“故障恢复”。先确认是不是采集器、标签、探针或日志链路出了问题。

### 优先检查的维度

- `system_*`
- `cpu_usage_*`
- `mem_*`
- `diskio_*`
- `net_*`
- `docker_container_*`
- `evidence_chain_process_*`
- `net_response_*`
- `sniffer_up`
- `kernel_log_up`

### 关键判断

- 多个指标族同时消失：优先怀疑采集黑屏、重启或网络隔离
- 只有一个指标族消失：优先怀疑局部 telemetry 丢失
- 探活挂了但主机指标还在：更像服务故障，不像主机黑屏

### 本步记录

- 哪些指标族在窗口内存在
- 哪些指标族缺失但理论上应该存在
- 有没有标签漂移、重复标签、stationid 变化

## 步骤 3：区分重启、黑屏和标签漂移

### 目标

先判断是不是生命周期事件，否则后面的 `rate()`、吞吐尖峰、计数器跳变都可能是错觉。

### 重点指标

- `system_uptime`
- `docker_container_status_uptime`
- `docker_container_status_started_at`
- `evidence_chain_process_start_time`
- `changes(...)`

### 判断方法

- `uptime` 回退或 `started_at` 变化：优先判定为重启
- 指标全体中断，然后恢复：优先判定为黑屏或采集恢复
- 某个进程 PID 变化、容器时间重置：判定局部生命周期变化

```tsdiag
{"collector":"proc_uptime"}
```

```tsdiag
{"collector":"recent_process_starts","limit":20}
```

### 本步记录

- 明确写出“宿主机重启 / 容器重启 / 仅 telemetry 黑屏 / 未发现生命周期变化”
- 记录生命周期边界前后 5 分钟的主机压力变化

## 步骤 4：检查资源瓶颈

### 目标

确认是 CPU、内存、IO、网络中的哪一类机制先出现，再去找影响对象。

### CPU 维度

- `system_load1`
- `cpu_usage_active`
- `cpu_usage_system`
- `cpu_usage_iowait`
- `cpu_usage_steal`
- `system_pressure_cpu_some_avg10`

### 内存维度

- `mem_used_percent`
- `mem_swap_free`
- `system_pressure_memory_some_avg10`
- `system_pressure_memory_full_avg10`
- `kernel_vmstat_pgmajfault`
- `kernel_vmstat_pswpin`
- `kernel_vmstat_pswpout`
- `kernel_vmstat_allocstall`
- `kernel_vmstat_oom_kill`

### IO 维度

- `diskio_read_bytes`
- `diskio_write_bytes`
- `diskio_io_time`
- `diskio_weighted_io_time`
- `diskio_iops_in_progress`
- `system_pressure_io_some_avg10`
- `system_pressure_io_full_avg10`
- `kernel_log_blocked_task_total`
- `kernel_log_io_error_total`

### 网络维度

- `net_drop_in`
- `net_drop_out`
- `net_err_in`
- `net_err_out`
- `netstat_tcpext_TCPTimeouts`
- `netstat_tcpext_TCPSynRetrans`
- `netstat_tcpext_TCPAbortOnTimeout`
- `netstat_tcpext_TCPMemoryPressures`
- `netstat_tcpext_TCPBacklogDrop`

### 本步记录

- 哪个维度最早出现持续性压力
- 哪个维度只有次生影响，没有机制证据
- 是否已经满足“资源瓶颈”的最低判断门槛

## 步骤 5：定位进程和容器热点

### 目标

资源异常成立后，再找谁在制造压力，谁在承受影响。

### 优先使用 evidence_chain

- `evidence_chain_process_cpu_percent`
- `evidence_chain_process_memory_mb`
- `evidence_chain_process_minor_faults_total`
- `evidence_chain_process_major_faults_total`
- `evidence_chain_process_io_read_bytes`
- `evidence_chain_process_io_write_bytes`
- `evidence_chain_process_io_read_latency_seconds_sum`
- `evidence_chain_process_deleted_file_handle_count`
- `evidence_chain_process_open_file_hotspot_top_count`
- `evidence_chain_process_start_time`

### 容器维度

- `docker_container_cpu_usage_percent`
- `docker_container_mem_usage_percent`
- `docker_container_memory_events_oom`
- `docker_container_memory_events_oom_kill`
- `docker_container_status_oom_killed`
- `docker_container_blkio_delay_seconds_total`

### 结合日志明细

如果 `evidence_chain` 已写入进程明细日志，优先补齐这些上下文：

- `cmdline`
- `exe`
- `container_id`
- `container_name`
- `container_image`
- 打开的热点文件
- 监听端口

```tsdiag
{"collector":"top_processes","sort":"cpu","limit":15}
```

```tsdiag
{"collector":"open_file_overview","limit":30,"sample_per_process":2}
```

### 本步记录

- TopN 热点进程
- 热点是否和受影响服务属于同一组件
- 热点是前因还是后果

## 步骤 6：构建时间线和因果链

### 目标

强制区分前兆、瓶颈、影响和观测缺口，不要把所有异常混成一锅。

### 推荐时间线字段

- `timestamp`
- `dimension`
- `metric`
- `direction`
- `interpretation`
- `confidence`

### 重点要求

- 先写前兆，再写压力积累，再写服务影响，再写恢复
- 每个根因候选至少要有一个机制信号和一个影响信号
- 如果第一名和第二名分差不足 3 分，只能说“最可能根因”

### 本步记录

- 确认信号
- 次生效应
- 根因候选
- 被排除解释

## 步骤 7：输出结论和下一步动作

### 目标

输出要能复盘、能继续排查、能回流到知识库，而不是一句模糊判断。

### 输出要求

- 使用绝对时间和时区
- 写清楚已经确认的事实
- 写清楚仍不确定的部分
- 写清楚缺哪些证据
- 写清楚下一步最值钱的检查项

### 结论模板

- `Window`
- `Metrics Covered`
- `Timeline`
- `Confirmed Signals`
- `Causal Chain`
- `Candidate Scorecard`
- `Root Cause Candidates`
- `Rejected Explanations`
- `Most Likely Root Cause`
- `Confidence`
- `Missing Evidence`
- `Next Checks`

## 备注

这份手册优先解决“主机/容器/进程/探活”统一排查的问题。单个中间件自身故障，比如 MySQL 或 Redis 的内部慢点，还需要额外补一层组件级 playbook。
