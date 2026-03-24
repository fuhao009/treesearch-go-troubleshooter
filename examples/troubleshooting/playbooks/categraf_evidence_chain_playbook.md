---
playbook_id: categraf_evidence_chain
scope: process-and-container
plugin: inputs/evidence_chain
---

# Categraf evidence_chain 使用手册

## 适用场景

当你已经怀疑问题和某个进程、容器、打开文件、网络连接或块读时延有关时，优先使用这份手册。

## 步骤 1：确认插件是否正常采集

### 目标

先确认 `evidence_chain` 不是空跑。

### 关键指标

- `evidence_chain_process_io_read_latency_up`
- `evidence_chain_process_cpu_percent`
- `evidence_chain_process_memory_mb`

### 关键判断

- 如果只有 `process_io_read_latency_up=0`，说明 eBPF 读时延采集不可用，但其他进程指标仍可用
- 如果整组 `evidence_chain_process_*` 都没有，先回头检查配置、权限、采集周期和 writer

### 内建采集动作

```tsdiag
{"collector":"host_identity"}
```

```tsdiag
{"collector":"process_match","match":["categraf","telegraf"],"sort":"cpu","limit":10,"include_cmdline":true}
```

### 本步记录

- 插件是否启用
- 采集周期
- 是否存在进程级 time series

## 步骤 2：按热点进程排序

### 目标

优先找最像“压力来源”的进程。

### 建议关注的字段

- `cpu_percent`
- `memory_mb`
- `thread_count`
- `minor_faults_total`
- `major_faults_total`
- `io_read_bytes`
- `io_write_bytes`
- `io_read_count`
- `io_write_count`

### 推荐判断方式

- CPU 高且持续：先看 `cpu_percent` 排序
- 读写压高：先看 `io_*` 和 `io_read_latency_seconds_sum`
- 内存紧张：先看 `memory_mb`、`major_faults_total`

### 内建采集动作

```tsdiag
{"collector":"top_processes","sort":"cpu","limit":15,"include_cmdline":true}
```

```tsdiag
{"collector":"open_file_overview","limit":20,"sample_per_process":2}
```

### 本步记录

- Top 5 进程
- 排序字段
- 这些进程是否在故障前就已经很高

## 步骤 3：结合进程明细日志补上下文

### 目标

指标只能告诉你“谁异常”，日志明细告诉你“它到底是什么进程”。

### 优先补齐的信息

- `cmdline`
- `exe`
- `process_type`
- `containerized`
- `docker.container_id`
- `docker.container_name`
- `docker.container_image`
- `files.deleted_open_file_count`
- `files.hotspots`
- 监听端口

### 典型用法

- 发现热点进程 PID 后，到 VictoriaLogs 里取同 PID 的 JSON 明细
- 用热点文件判断是不是 checkpoint、binlog、WAL、日志刷盘、临时文件暴涨
- 用监听端口和容器名，把异常进程绑定到具体业务组件

### 本步记录

- 进程身份
- 容器身份
- 热点文件
- 监听端口

## 步骤 4：结合块读时延判断是否存在 IO 卡顿

### 目标

不要只看字节量，还要看请求级读时延。

### 关键指标

- `evidence_chain_process_io_read_latency_collect_ok`
- `evidence_chain_process_io_read_latency_seconds_count`
- `evidence_chain_process_io_read_latency_seconds_sum`
- `evidence_chain_process_io_read_latency_seconds_bucket`

### 典型判断

- 时延 count 持续增长，sum 增长更快：说明平均读时延升高
- 直方图高桶累积：说明存在明显慢读
- 如果宿主机 `cpu_usage_iowait` 也升高，IO 瓶颈可信度明显上升

### 本步记录

- 哪些进程出现块读慢
- 是否与业务影响时间对齐
- 是否能和主机 IO 信号互相印证

## 步骤 5：把 evidence_chain 放回根因链路里

### 目标

`evidence_chain` 用来“识别 actor”，不是单独给最终结论。

### 推荐联动方式

- 和主机 CPU/内存/磁盘/网络信号联动
- 和 `docker_container_*` 生命周期联动
- 和 `net_response_*` 或业务探活联动

### 常见结论模式

- 宿主机 IO 压力 + 某个数据库进程读时延高 + 业务探活超时
- 内存回收/major fault 增加 + 某个 JVM/Go 进程内存占用大 + 容器 OOM
- 网络超时上升 + 进程 socket/连接数异常 + 探活失败

### 本步记录

- 这个进程是根因候选、触发者，还是受害者
- 证据来自哪些维度
- 还缺什么证据

## 关键指标速查

### 资源热点

- `evidence_chain_process_cpu_percent`
- `evidence_chain_process_memory_mb`
- `evidence_chain_process_thread_count`

### 缺页与内存压力

- `evidence_chain_process_minor_faults_total`
- `evidence_chain_process_major_faults_total`

### IO 与慢读

- `evidence_chain_process_io_read_bytes`
- `evidence_chain_process_io_write_bytes`
- `evidence_chain_process_io_read_latency_seconds_sum`
- `evidence_chain_process_io_read_latency_seconds_bucket`

### 文件与句柄

- `evidence_chain_process_deleted_file_handle_count`
- `evidence_chain_process_open_file_hotspot_unique`
- `evidence_chain_process_open_file_hotspot_top_count`

### 生命周期

- `evidence_chain_process_start_time`
