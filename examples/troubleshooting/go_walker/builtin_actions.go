package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const procClockTicksPerSecond = 100.0

type builtinActionSpec struct {
	Collector        string   `json:"collector"`
	Limit            int      `json:"limit,omitempty"`
	Sort             string   `json:"sort,omitempty"`
	Match            []string `json:"match,omitempty"`
	IncludeCmdline   bool     `json:"include_cmdline,omitempty"`
	SamplePerProcess int      `json:"sample_per_process,omitempty"`
}

type processInfo struct {
	PID        int
	PPID       int
	Command    string
	Cmdline    string
	StartTime  time.Time
	CPUPercent float64
	RSSBytes   uint64
	Threads    int
	FDCount    int
}

type mountInfo struct {
	Device     string
	MountPoint string
	FSType     string
}

type netDevStats struct {
	Name      string
	RXBytes   uint64
	RXPackets uint64
	RXErrors  uint64
	RXDrops   uint64
	TXBytes   uint64
	TXPackets uint64
	TXErrors  uint64
	TXDrops   uint64
}

func isSupportedActionLang(lang string) bool {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "tsdiag", "diag", "godiag":
		return true
	default:
		return false
	}
}

func parseBuiltinActionSpec(text string) (builtinActionSpec, error) {
	var spec builtinActionSpec
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return builtinActionSpec{}, err
	}
	spec.Collector = strings.TrimSpace(spec.Collector)
	spec.Sort = strings.ToLower(strings.TrimSpace(spec.Sort))
	spec.Match = cleanStringList(spec.Match)
	if spec.Collector == "" {
		return builtinActionSpec{}, fmt.Errorf("collector is required")
	}
	return spec, nil
}

func marshalBuiltinActionSpec(spec builtinActionSpec) string {
	payload, err := json.Marshal(spec)
	if err != nil {
		return spec.Collector
	}
	return string(payload)
}

func runBuiltinAction(ctx context.Context, spec builtinActionSpec) (string, error) {
	switch spec.Collector {
	case "host_identity":
		return collectHostIdentity(ctx), nil
	case "proc_uptime":
		return collectProcUptime(ctx)
	case "scheduler_overview":
		return collectSchedulerOverview(ctx)
	case "memory_overview":
		return collectMemoryOverview(ctx)
	case "filesystem_overview":
		return collectFilesystemOverview(ctx, spec)
	case "network_overview":
		return collectNetworkOverview(ctx)
	case "top_processes":
		return collectTopProcesses(ctx, spec)
	case "process_match":
		return collectMatchedProcesses(ctx, spec)
	case "recent_process_starts":
		return collectRecentProcessStarts(ctx, spec)
	case "open_file_overview":
		return collectOpenFileOverview(ctx, spec)
	default:
		return "", fmt.Errorf("unsupported collector: %s", spec.Collector)
	}
}

func collectHostIdentity(ctx context.Context) string {
	now := time.Now()
	hostname, _ := os.Hostname()
	uptimeSeconds, _ := readProcUptimeSeconds()
	load1, load5, load15, running, total, _ := readLoadAvg()

	lines := []string{
		fmt.Sprintf("current_time=%s", now.Format("2006-01-02T15:04:05-0700")),
		fmt.Sprintf("hostname=%s", firstNonEmpty(hostname, "unknown")),
	}
	if uptimeSeconds > 0 {
		lines = append(lines,
			fmt.Sprintf("uptime_seconds=%.2f", uptimeSeconds),
			fmt.Sprintf("uptime_human=%s", formatDuration(time.Duration(uptimeSeconds*float64(time.Second)))),
		)
	}
	if load1 != "" {
		lines = append(lines,
			fmt.Sprintf("load1=%s", load1),
			fmt.Sprintf("load5=%s", load5),
			fmt.Sprintf("load15=%s", load15),
		)
	}
	if running != "" && total != "" {
		lines = append(lines, fmt.Sprintf("runnable_tasks=%s/%s", running, total))
	}
	return strings.Join(lines, "\n")
}

func collectProcUptime(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	uptimeSeconds, err := readProcUptimeSeconds()
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		fmt.Sprintf("uptime_seconds=%.2f", uptimeSeconds),
		fmt.Sprintf("uptime_human=%s", formatDuration(time.Duration(uptimeSeconds*float64(time.Second)))),
	}, "\n"), nil
}

func collectSchedulerOverview(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	lines := []string{}
	load1, load5, load15, running, total, err := readLoadAvg()
	if err == nil {
		lines = append(lines,
			fmt.Sprintf("load1=%s", load1),
			fmt.Sprintf("load5=%s", load5),
			fmt.Sprintf("load15=%s", load15),
		)
		if running != "" && total != "" {
			lines = append(lines, fmt.Sprintf("runnable_tasks=%s/%s", running, total))
		}
	}

	if statMap, err := readProcStatCounters(); err == nil {
		if value, ok := statMap["procs_running"]; ok {
			lines = append(lines, fmt.Sprintf("procs_running=%d", value))
		}
		if value, ok := statMap["procs_blocked"]; ok {
			lines = append(lines, fmt.Sprintf("procs_blocked=%d", value))
		}
	}

	if meminfo, err := readMemInfo(); err == nil {
		if value, ok := meminfo["MemAvailable"]; ok {
			lines = append(lines, fmt.Sprintf("mem_available_mb=%d", value/1024))
		}
		if value, ok := meminfo["MemFree"]; ok {
			lines = append(lines, fmt.Sprintf("mem_free_mb=%d", value/1024))
		}
	}

	if len(lines) == 0 {
		return "", fmt.Errorf("scheduler overview unavailable")
	}
	return strings.Join(lines, "\n"), nil
}

func collectMemoryOverview(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	meminfo, err := readMemInfo()
	if err != nil {
		return "", err
	}

	memTotal := meminfo["MemTotal"]
	memAvailable := meminfo["MemAvailable"]
	memFree := meminfo["MemFree"]
	memUsed := uint64(0)
	if memTotal > memAvailable {
		memUsed = memTotal - memAvailable
	}
	swapTotal := meminfo["SwapTotal"]
	swapFree := meminfo["SwapFree"]
	swapUsed := uint64(0)
	if swapTotal > swapFree {
		swapUsed = swapTotal - swapFree
	}

	lines := []string{
		fmt.Sprintf("mem_total_mb=%d", memTotal/1024),
		fmt.Sprintf("mem_used_mb=%d", memUsed/1024),
		fmt.Sprintf("mem_available_mb=%d", memAvailable/1024),
		fmt.Sprintf("mem_free_mb=%d", memFree/1024),
		fmt.Sprintf("swap_total_mb=%d", swapTotal/1024),
		fmt.Sprintf("swap_used_mb=%d", swapUsed/1024),
		fmt.Sprintf("swap_free_mb=%d", swapFree/1024),
	}
	return strings.Join(lines, "\n"), nil
}

func collectFilesystemOverview(ctx context.Context, spec builtinActionSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mounts, err := readMounts()
	if err != nil {
		return "", err
	}
	limit := spec.Limit
	if limit <= 0 {
		limit = 10
	}

	lines := []string{"MOUNT FSTYPE TOTAL USED AVAIL USE%"}
	count := 0
	for _, mount := range mounts {
		if count >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount.MountPoint, &stat); err != nil {
			continue
		}
		total := uint64(stat.Blocks) * uint64(stat.Bsize)
		avail := uint64(stat.Bavail) * uint64(stat.Bsize)
		free := uint64(stat.Bfree) * uint64(stat.Bsize)
		used := uint64(0)
		if total > free {
			used = total - free
		}
		usePercent := 0.0
		if total > 0 {
			usePercent = float64(used) / float64(total) * 100
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s %s %.1f%%",
			mount.MountPoint,
			mount.FSType,
			formatBytes(total),
			formatBytes(used),
			formatBytes(avail),
			usePercent,
		))
		count++
	}
	return strings.Join(lines, "\n"), nil
}

func collectNetworkOverview(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	lines := []string{}
	devs, err := readNetDevStats()
	if err == nil && len(devs) > 0 {
		lines = append(lines, "IFACE RX_BYTES TX_BYTES RX_DROP TX_DROP RX_ERR TX_ERR")
		sort.Slice(devs, func(i, j int) bool {
			left := devs[i].RXBytes + devs[i].TXBytes
			right := devs[j].RXBytes + devs[j].TXBytes
			if left == right {
				return devs[i].Name < devs[j].Name
			}
			return left > right
		})
		for _, dev := range devs {
			if strings.HasPrefix(dev.Name, "lo") {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s %s %s %d %d %d %d",
				dev.Name,
				formatBytes(dev.RXBytes),
				formatBytes(dev.TXBytes),
				dev.RXDrops,
				dev.TXDrops,
				dev.RXErrors,
				dev.TXErrors,
			))
		}
	}

	selected := map[string]uint64{}
	for _, file := range []string{"/proc/net/snmp", "/proc/net/netstat"} {
		values, err := readKeyValueProcTable(file)
		if err != nil {
			continue
		}
		for _, key := range []string{
			"Ip.InDiscards",
			"Ip.OutDiscards",
			"Tcp.RetransSegs",
			"Tcp.InErrs",
			"Tcp.OutRsts",
			"TcpExt.TCPTimeouts",
			"TcpExt.TCPSynRetrans",
			"TcpExt.TCPAbortOnTimeout",
			"TcpExt.TCPBacklogDrop",
			"TcpExt.TCPMemoryPressures",
		} {
			if value, ok := values[key]; ok {
				selected[key] = value
			}
		}
	}
	if len(selected) > 0 {
		lines = append(lines, "", "NETWORK_COUNTERS")
		keys := make([]string, 0, len(selected))
		for key := range selected {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("%s=%d", key, selected[key]))
		}
	}

	if len(lines) == 0 {
		return "", fmt.Errorf("network overview unavailable")
	}
	return strings.Join(lines, "\n"), nil
}

func collectTopProcesses(ctx context.Context, spec builtinActionSpec) (string, error) {
	processes, err := collectProcessInfos(ctx, spec.IncludeCmdline, false)
	if err != nil {
		return "", err
	}
	sortProcessInfos(processes, firstNonEmpty(spec.Sort, "cpu"))
	limit := spec.Limit
	if limit <= 0 {
		limit = 15
	}
	if len(processes) > limit {
		processes = processes[:limit]
	}

	lines := []string{"PID PPID CPU% RSS THREADS START COMMAND"}
	for _, item := range processes {
		commandText := item.Command
		if spec.IncludeCmdline && strings.TrimSpace(item.Cmdline) != "" {
			commandText = item.Cmdline
		}
		lines = append(lines, fmt.Sprintf("%d %d %.1f %s %d %s %s",
			item.PID,
			item.PPID,
			item.CPUPercent,
			formatBytes(item.RSSBytes),
			item.Threads,
			item.StartTime.Format("2006-01-02T15:04:05"),
			limitText(commandText, 160),
		))
	}
	return strings.Join(lines, "\n"), nil
}

func collectMatchedProcesses(ctx context.Context, spec builtinActionSpec) (string, error) {
	processes, err := collectProcessInfos(ctx, true, false)
	if err != nil {
		return "", err
	}
	matchTerms := make([]string, 0, len(spec.Match))
	for _, item := range spec.Match {
		item = normalize(item)
		if item != "" {
			matchTerms = append(matchTerms, item)
		}
	}
	if len(matchTerms) == 0 {
		return "", fmt.Errorf("match is required for process_match")
	}

	filtered := make([]processInfo, 0)
	for _, item := range processes {
		haystack := normalize(item.Command + " " + item.Cmdline)
		for _, term := range matchTerms {
			if strings.Contains(haystack, term) {
				filtered = append(filtered, item)
				break
			}
		}
	}
	sortProcessInfos(filtered, firstNonEmpty(spec.Sort, "cpu"))
	limit := spec.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	lines := []string{"PID PPID CPU% RSS THREADS START COMMAND"}
	for _, item := range filtered {
		commandText := item.Command
		if strings.TrimSpace(item.Cmdline) != "" {
			commandText = item.Cmdline
		}
		lines = append(lines, fmt.Sprintf("%d %d %.1f %s %d %s %s",
			item.PID,
			item.PPID,
			item.CPUPercent,
			formatBytes(item.RSSBytes),
			item.Threads,
			item.StartTime.Format("2006-01-02T15:04:05"),
			limitText(commandText, 160),
		))
	}
	if len(filtered) == 0 {
		lines = append(lines, "no_matching_processes")
	}
	return strings.Join(lines, "\n"), nil
}

func collectRecentProcessStarts(ctx context.Context, spec builtinActionSpec) (string, error) {
	processes, err := collectProcessInfos(ctx, false, false)
	if err != nil {
		return "", err
	}
	sort.Slice(processes, func(i, j int) bool {
		if processes[i].StartTime.Equal(processes[j].StartTime) {
			return processes[i].PID > processes[j].PID
		}
		return processes[i].StartTime.After(processes[j].StartTime)
	})
	limit := spec.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(processes) > limit {
		processes = processes[:limit]
	}

	lines := []string{"PID START COMMAND"}
	for _, item := range processes {
		lines = append(lines, fmt.Sprintf("%d %s %s",
			item.PID,
			item.StartTime.Format("2006-01-02T15:04:05"),
			limitText(item.Command, 120),
		))
	}
	return strings.Join(lines, "\n"), nil
}

func collectOpenFileOverview(ctx context.Context, spec builtinActionSpec) (string, error) {
	processes, err := collectProcessInfos(ctx, true, true)
	if err != nil {
		return "", err
	}
	sort.Slice(processes, func(i, j int) bool {
		if processes[i].FDCount == processes[j].FDCount {
			return processes[i].PID < processes[j].PID
		}
		return processes[i].FDCount > processes[j].FDCount
	})
	limit := spec.Limit
	if limit <= 0 {
		limit = 20
	}
	samplePerProcess := spec.SamplePerProcess
	if samplePerProcess <= 0 {
		samplePerProcess = 3
	}
	if len(processes) > limit {
		processes = processes[:limit]
	}

	lines := []string{"PID FD_COUNT COMMAND"}
	for _, item := range processes {
		commandText := firstNonEmpty(item.Cmdline, item.Command)
		lines = append(lines, fmt.Sprintf("%d %d %s", item.PID, item.FDCount, limitText(commandText, 160)))
		targets := sampleFDTargets(item.PID, samplePerProcess)
		for _, target := range targets {
			lines = append(lines, fmt.Sprintf("  - %s", limitText(target, 160)))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func collectProcessInfos(ctx context.Context, includeCmdline, includeFDCount bool) ([]processInfo, error) {
	uptimeSeconds, err := readProcUptimeSeconds()
	if err != nil {
		return nil, err
	}
	bootTime, err := readBootTime()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	processes := make([]processInfo, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := readProcessInfo(pid, bootTime, uptimeSeconds, includeCmdline, includeFDCount)
		if err != nil {
			continue
		}
		processes = append(processes, info)
	}
	return processes, nil
}

func readProcessInfo(pid int, bootTime time.Time, uptimeSeconds float64, includeCmdline, includeFDCount bool) (processInfo, error) {
	statLine, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processInfo{}, err
	}
	command, ppid, utimeTicks, stimeTicks, threads, startTicks, rssPages, err := parseProcStat(string(statLine))
	if err != nil {
		return processInfo{}, err
	}

	startTime := bootTime.Add(time.Duration(float64(startTicks)/procClockTicksPerSecond) * time.Second)
	processElapsed := uptimeSeconds - float64(startTicks)/procClockTicksPerSecond
	if processElapsed < 0.01 {
		processElapsed = 0.01
	}
	cpuPercent := (float64(utimeTicks+stimeTicks) / procClockTicksPerSecond) / processElapsed * 100
	rssBytes := uint64(rssPages) * uint64(os.Getpagesize())

	info := processInfo{
		PID:        pid,
		PPID:       ppid,
		Command:    command,
		StartTime:  startTime,
		CPUPercent: cpuPercent,
		RSSBytes:   rssBytes,
		Threads:    threads,
	}

	if includeCmdline {
		cmdline := readProcCmdline(pid)
		if strings.TrimSpace(cmdline) != "" {
			info.Cmdline = cmdline
		}
	}
	if includeFDCount {
		info.FDCount = countProcessFDs(pid)
	}
	return info, nil
}

func parseProcStat(line string) (command string, ppid int, utimeTicks uint64, stimeTicks uint64, threads int, startTicks uint64, rssPages int64, err error) {
	line = strings.TrimSpace(line)
	right := strings.LastIndex(line, ")")
	left := strings.Index(line, "(")
	if left < 0 || right < 0 || right <= left {
		err = fmt.Errorf("invalid proc stat line")
		return
	}
	command = line[left+1 : right]
	fields := strings.Fields(strings.TrimSpace(line[right+1:]))
	if len(fields) < 22 {
		err = fmt.Errorf("proc stat fields too short")
		return
	}

	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return
	}
	utimeTicks, err = strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return
	}
	stimeTicks, err = strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return
	}
	threads, err = strconv.Atoi(fields[17])
	if err != nil {
		return
	}
	startTicks, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return
	}
	rssPages, err = strconv.ParseInt(fields[21], 10, 64)
	return
}

func readProcCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	parts := strings.Split(string(data), "\x00")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, " ")
}

func countProcessFDs(pid int) int {
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return 0
	}
	return len(entries)
}

func sampleFDTargets(pid int, limit int) []string {
	if limit <= 0 {
		return nil
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, entry := range entries {
		if len(out) >= limit {
			break
		}
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

func sortProcessInfos(processes []processInfo, mode string) {
	switch mode {
	case "rss", "memory":
		sort.Slice(processes, func(i, j int) bool {
			if processes[i].RSSBytes == processes[j].RSSBytes {
				return processes[i].PID < processes[j].PID
			}
			return processes[i].RSSBytes > processes[j].RSSBytes
		})
	case "start":
		sort.Slice(processes, func(i, j int) bool {
			if processes[i].StartTime.Equal(processes[j].StartTime) {
				return processes[i].PID > processes[j].PID
			}
			return processes[i].StartTime.After(processes[j].StartTime)
		})
	default:
		sort.Slice(processes, func(i, j int) bool {
			if processes[i].CPUPercent == processes[j].CPUPercent {
				return processes[i].PID < processes[j].PID
			}
			return processes[i].CPUPercent > processes[j].CPUPercent
		})
	}
}

func readProcUptimeSeconds() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("invalid /proc/uptime")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readLoadAvg() (load1, load5, load15, running, total string, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "", "", "", "", "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 4 {
		return "", "", "", "", "", fmt.Errorf("invalid /proc/loadavg")
	}
	load1 = fields[0]
	load5 = fields[1]
	load15 = fields[2]
	parts := strings.SplitN(fields[3], "/", 2)
	if len(parts) == 2 {
		running = parts[0]
		total = parts[1]
	}
	return load1, load5, load15, running, total, nil
}

func readBootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			break
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		return time.Unix(seconds, 0), nil
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("btime not found")
}

func readProcStatCounters() (map[string]uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		if fields[0] != "procs_running" && fields[0] != "procs_blocked" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[fields[0]] = value
	}
	return out, scanner.Err()
}

func readMemInfo() (map[string]uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(parts[1]))
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		out[parts[0]] = value
	}
	return out, scanner.Err()
}

func readMounts() ([]mountInfo, error) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{
		"proc": true, "sysfs": true, "devpts": true, "tmpfs": true, "cgroup": true, "cgroup2": true,
		"mqueue": true, "hugetlbfs": true, "debugfs": true, "tracefs": true, "securityfs": true,
		"pstore": true, "configfs": true, "fusectl": true, "binfmt_misc": true,
	}
	seen := map[string]bool{}
	mounts := []mountInfo{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mount := mountInfo{
			Device:     fields[0],
			MountPoint: fields[1],
			FSType:     fields[2],
		}
		if skip[mount.FSType] || seen[mount.MountPoint] {
			continue
		}
		seen[mount.MountPoint] = true
		mounts = append(mounts, mount)
	}
	sort.Slice(mounts, func(i, j int) bool {
		return mounts[i].MountPoint < mounts[j].MountPoint
	})
	return mounts, scanner.Err()
}

func readNetDevStats() ([]netDevStats, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	stats := []netDevStats{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo <= 2 {
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		fields := strings.Fields(strings.TrimSpace(parts[1]))
		if len(fields) < 16 {
			continue
		}
		rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
		rxPackets, _ := strconv.ParseUint(fields[1], 10, 64)
		rxErrors, _ := strconv.ParseUint(fields[2], 10, 64)
		rxDrops, _ := strconv.ParseUint(fields[3], 10, 64)
		txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
		txPackets, _ := strconv.ParseUint(fields[9], 10, 64)
		txErrors, _ := strconv.ParseUint(fields[10], 10, 64)
		txDrops, _ := strconv.ParseUint(fields[11], 10, 64)
		stats = append(stats, netDevStats{
			Name:      name,
			RXBytes:   rxBytes,
			RXPackets: rxPackets,
			RXErrors:  rxErrors,
			RXDrops:   rxDrops,
			TXBytes:   txBytes,
			TXPackets: txPackets,
			TXErrors:  txErrors,
			TXDrops:   txDrops,
		})
	}
	return stats, scanner.Err()
}

func readKeyValueProcTable(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := map[string]uint64{}
	for i := 0; i+1 < len(lines); i += 2 {
		keys := strings.Fields(strings.TrimSpace(lines[i]))
		values := strings.Fields(strings.TrimSpace(lines[i+1]))
		if len(keys) != len(values) || len(keys) < 2 {
			continue
		}
		group := strings.TrimSuffix(keys[0], ":")
		for idx := 1; idx < len(keys); idx++ {
			value, err := strconv.ParseUint(values[idx], 10, 64)
			if err != nil {
				continue
			}
			out[group+"."+keys[idx]] = value
		}
	}
	return out, nil
}

func formatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	div, exp := uint64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(value)/float64(div), "KMGTPE"[exp])
}

func formatDuration(value time.Duration) string {
	if value <= 0 {
		return "0s"
	}
	value = value.Round(time.Second)
	days := value / (24 * time.Hour)
	value -= days * 24 * time.Hour
	hours := value / time.Hour
	value -= hours * time.Hour
	minutes := value / time.Minute
	value -= minutes * time.Minute
	seconds := value / time.Second

	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts, "")
}
