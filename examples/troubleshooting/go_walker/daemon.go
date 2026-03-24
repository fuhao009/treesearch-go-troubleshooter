package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type daemonJob struct {
	JobID                string            `json:"job_id"`
	Name                 string            `json:"name,omitempty"`
	Query                string            `json:"query,omitempty"`
	DocIDs               []string          `json:"doc_ids,omitempty"`
	ProbeAction          string            `json:"probe_action,omitempty"`
	ProbeText            string            `json:"probe_text,omitempty"`
	LegacyProbeCommand   string            `json:"probe_command,omitempty"`
	ProbeMergeMode       string            `json:"probe_merge_mode,omitempty"`
	SkipIfProbeEmpty     bool              `json:"skip_if_probe_empty,omitempty"`
	IntervalSeconds      int               `json:"interval_seconds"`
	TimeoutSeconds       int               `json:"timeout_seconds,omitempty"`
	MaxDocsPerCycle      int               `json:"max_docs_per_cycle,omitempty"`
	AutoGenerateTree     bool              `json:"auto_generate_tree,omitempty"`
	GenerationWindow     int               `json:"generation_window,omitempty"`
	MinRecordsToGenerate int               `json:"min_records_to_generate,omitempty"`
	Enabled              bool              `json:"enabled"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	CreatedAt            string            `json:"created_at,omitempty"`
	UpdatedAt            string            `json:"updated_at,omitempty"`
	Status               daemonJobStatus   `json:"status"`
}

type daemonJobStatus struct {
	State               string   `json:"state,omitempty"`
	LastExecutionStatus string   `json:"last_execution_status,omitempty"`
	LastRunAt           string   `json:"last_run_at,omitempty"`
	NextRunAt           string   `json:"next_run_at,omitempty"`
	LastError           string   `json:"last_error,omitempty"`
	LastQuery           string   `json:"last_query,omitempty"`
	LastProbeOutput     string   `json:"last_probe_output,omitempty"`
	LastRecordIDs       []string `json:"last_record_ids,omitempty"`
	LastRoutedDocs      []string `json:"last_routed_docs,omitempty"`
	LastGeneratedDocIDs []string `json:"last_generated_doc_ids,omitempty"`
	ActiveRunID         string   `json:"active_run_id,omitempty"`
	RunCount            int      `json:"run_count"`
}

type daemonJobUpsertRequest struct {
	JobID                string            `json:"job_id,omitempty"`
	Name                 string            `json:"name,omitempty"`
	Query                string            `json:"query,omitempty"`
	DocIDs               []string          `json:"doc_ids,omitempty"`
	ProbeAction          string            `json:"probe_action,omitempty"`
	ProbeText            string            `json:"probe_text,omitempty"`
	LegacyProbeCommand   string            `json:"probe_command,omitempty"`
	ProbeMergeMode       string            `json:"probe_merge_mode,omitempty"`
	SkipIfProbeEmpty     *bool             `json:"skip_if_probe_empty,omitempty"`
	IntervalSeconds      int               `json:"interval_seconds,omitempty"`
	TimeoutSeconds       int               `json:"timeout_seconds,omitempty"`
	MaxDocsPerCycle      int               `json:"max_docs_per_cycle,omitempty"`
	AutoGenerateTree     *bool             `json:"auto_generate_tree,omitempty"`
	GenerationWindow     int               `json:"generation_window,omitempty"`
	MinRecordsToGenerate int               `json:"min_records_to_generate,omitempty"`
	Enabled              *bool             `json:"enabled,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
}

type daemonJobRuntime struct {
	job     daemonJob
	running bool
}

type daemonManager struct {
	server       *apiServer
	generatedDir string
	tickInterval time.Duration
	startedAt    time.Time

	mu   sync.Mutex
	jobs map[string]*daemonJobRuntime

	stopCh chan struct{}
	doneCh chan struct{}
}

type routedDocument struct {
	Doc   Document
	Score int
	Rank  int
}

type generatedStepGroup struct {
	Title        string
	Text         string
	ActionSpec   string
	Total        int
	Failed       int
	Succeeded    int
	LatestSeenAt string
	OutputCounts map[string]int
}

func normalizeProbeInput(actionSpec, probeText, legacy string) (string, string) {
	actionSpec = strings.TrimSpace(actionSpec)
	probeText = strings.TrimSpace(probeText)
	legacy = strings.TrimSpace(legacy)

	if actionSpec != "" || probeText != "" {
		return actionSpec, probeText
	}
	if legacy == "" {
		return "", ""
	}
	if _, err := parseBuiltinActionSpec(legacy); err == nil {
		return strings.TrimSpace(legacy), ""
	}
	return "", decodeLegacyProbeText(legacy)
}

func buildLiteralProbeActionRun(text string) ActionRun {
	return ActionRun{
		Engine:   "inline_text",
		Spec:     `{"kind":"probe_text"}`,
		ExitCode: 0,
		Output:   strings.TrimSpace(text),
		RanAt:    time.Now().UTC().Format(time.RFC3339),
	}
}

func newDaemonManager(server *apiServer, generatedDir string, tickInterval time.Duration) (*daemonManager, error) {
	if tickInterval <= 0 {
		tickInterval = 5 * time.Second
	}
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		return nil, err
	}

	jobs, err := loadDaemonJobsFromDB(server.db)
	if err != nil {
		return nil, err
	}

	manager := &daemonManager{
		server:       server,
		generatedDir: generatedDir,
		tickInterval: tickInterval,
		startedAt:    time.Now().UTC(),
		jobs:         make(map[string]*daemonJobRuntime, len(jobs)),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}

	for _, job := range jobs {
		job = normalizeLoadedDaemonJob(job)
		manager.jobs[job.JobID] = &daemonJobRuntime{job: job}
	}
	return manager, nil
}

func (d *daemonManager) Start() {
	go d.loop()
}

func (d *daemonManager) Stop() {
	close(d.stopCh)
	<-d.doneCh
}

func (d *daemonManager) loop() {
	defer close(d.doneCh)

	d.dispatchDueJobs()
	ticker := time.NewTicker(d.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.dispatchDueJobs()
		case <-d.stopCh:
			return
		}
	}
}

func (d *daemonManager) dispatchDueJobs() {
	now := time.Now().UTC()
	toRun := make([]daemonJob, 0)

	d.mu.Lock()
	for _, runtime := range d.jobs {
		if !runtime.job.Enabled || runtime.running {
			continue
		}
		nextRunAt := parseRFC3339(runtime.job.Status.NextRunAt)
		if !nextRunAt.IsZero() && nextRunAt.After(now) {
			continue
		}
		runtime.running = true
		runtime.job.Status.State = "running"
		runtime.job.Status.ActiveRunID = fmt.Sprintf("%s_%d", runtime.job.JobID, now.UnixNano())
		runtime.job.Status.LastError = ""
		toRun = append(toRun, runtime.job)
	}
	d.mu.Unlock()

	for _, job := range toRun {
		_ = saveDaemonJobToDB(d.server.db, job)
		go d.runJob(job)
	}
}

func (d *daemonManager) ListJobs() []daemonJob {
	d.mu.Lock()
	defer d.mu.Unlock()

	items := make([]daemonJob, 0, len(d.jobs))
	for _, runtime := range d.jobs {
		items = append(items, runtime.job)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt == items[j].UpdatedAt {
			return items[i].JobID < items[j].JobID
		}
		return items[i].UpdatedAt > items[j].UpdatedAt
	})
	return items
}

func (d *daemonManager) GetJob(jobID string) (daemonJob, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	runtime, ok := d.jobs[jobID]
	if !ok {
		return daemonJob{}, false
	}
	return runtime.job, true
}

func (d *daemonManager) UpsertJob(req daemonJobUpsertRequest) (daemonJob, error) {
	d.mu.Lock()
	var existing *daemonJob
	if runtime, ok := d.jobs[req.JobID]; ok && strings.TrimSpace(req.JobID) != "" {
		copyJob := runtime.job
		existing = &copyJob
	}
	d.mu.Unlock()

	job, err := buildDaemonJob(req, existing)
	if err != nil {
		return daemonJob{}, err
	}

	d.mu.Lock()
	runtime, ok := d.jobs[job.JobID]
	if ok {
		runtime.job = job
	} else {
		runtime = &daemonJobRuntime{job: job}
		d.jobs[job.JobID] = runtime
	}
	snapshot := runtime.job
	d.mu.Unlock()

	if err := saveDaemonJobToDB(d.server.db, snapshot); err != nil {
		return daemonJob{}, err
	}
	return snapshot, nil
}

func (d *daemonManager) StartJob(jobID string) (daemonJob, error) {
	return d.setJobEnabled(jobID, true)
}

func (d *daemonManager) StopJob(jobID string) (daemonJob, error) {
	return d.setJobEnabled(jobID, false)
}

func (d *daemonManager) TriggerJob(jobID string) (daemonJob, error) {
	d.mu.Lock()
	runtime, ok := d.jobs[jobID]
	if !ok {
		d.mu.Unlock()
		return daemonJob{}, sql.ErrNoRows
	}
	runtime.job.Enabled = true
	runtime.job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	runtime.job.Status.State = "waiting"
	runtime.job.Status.NextRunAt = time.Now().UTC().Format(time.RFC3339)
	snapshot := runtime.job
	d.mu.Unlock()

	if err := saveDaemonJobToDB(d.server.db, snapshot); err != nil {
		return daemonJob{}, err
	}
	d.dispatchDueJobs()
	return snapshot, nil
}

func (d *daemonManager) setJobEnabled(jobID string, enabled bool) (daemonJob, error) {
	d.mu.Lock()
	runtime, ok := d.jobs[jobID]
	if !ok {
		d.mu.Unlock()
		return daemonJob{}, sql.ErrNoRows
	}
	now := time.Now().UTC().Format(time.RFC3339)
	runtime.job.Enabled = enabled
	runtime.job.UpdatedAt = now
	if enabled {
		runtime.job.Status.State = "waiting"
		runtime.job.Status.NextRunAt = now
	} else {
		runtime.job.Status.State = "stopped"
		runtime.job.Status.NextRunAt = ""
		runtime.job.Status.ActiveRunID = ""
	}
	snapshot := runtime.job
	d.mu.Unlock()

	if err := saveDaemonJobToDB(d.server.db, snapshot); err != nil {
		return daemonJob{}, err
	}
	if enabled {
		d.dispatchDueJobs()
	}
	return snapshot, nil
}

func (d *daemonManager) Status() map[string]any {
	jobs := d.ListJobs()
	active := 0
	enabled := 0
	for _, job := range jobs {
		if job.Enabled {
			enabled++
		}
		if job.Status.State == "running" {
			active++
		}
	}
	return map[string]any{
		"component":           "daemon_scheduler",
		"started_at":          d.startedAt.Format(time.RFC3339),
		"job_count":           len(jobs),
		"enabled_job_count":   enabled,
		"active_job_count":    active,
		"tick_interval":       d.tickInterval.String(),
		"generated_tree_dir":  d.generatedDir,
		"jobs":                jobs,
		"continuous_enabled":  true,
		"auto_generate_trees": true,
	}
}

func (d *daemonManager) runJob(job daemonJob) {
	startedAt := time.Now().UTC()
	lastQuery := strings.TrimSpace(job.Query)
	lastProbeOutput := ""
	lastRecordIDs := []string{}
	lastRoutedDocs := []string{}
	lastGeneratedDocIDs := []string{}
	lastExecutionStatus := "ok"
	lastError := ""

	defer func() {
		d.finishJobRun(job.JobID, startedAt, lastQuery, lastProbeOutput, lastRecordIDs, lastRoutedDocs, lastGeneratedDocIDs, lastExecutionStatus, lastError)
	}()

	probeStep, effectiveQuery, probeOutput, skipped, probeErr := buildProbePreludeStep(job)
	lastQuery = effectiveQuery
	lastProbeOutput = limitText(strings.TrimSpace(probeOutput), 1200)
	if probeErr != nil {
		lastExecutionStatus = "error"
		lastError = probeErr.Error()
		return
	}
	if skipped {
		lastExecutionStatus = "skipped"
		return
	}

	docs, err := loadDocumentsFromDB(d.server.db)
	if err != nil {
		lastExecutionStatus = "error"
		lastError = err.Error()
		return
	}

	routedDocs, err := resolveDocumentsForJob(d.server.db, docs, job, effectiveQuery)
	if err != nil {
		lastExecutionStatus = "error"
		lastError = err.Error()
		return
	}
	if len(routedDocs) == 0 {
		lastExecutionStatus = "skipped"
		lastError = "no runnable troubleshooting tree matched"
		return
	}

	for _, routed := range routedDocs {
		lastRoutedDocs = append(lastRoutedDocs, routed.Doc.DocID)
		preludeSteps := make([]experienceStep, 0, 2)
		if probeStep != nil {
			preludeSteps = append(preludeSteps, *probeStep)
		}
		preludeSteps = append(preludeSteps, buildRoutePreludeStep(job, routed, routedDocs, effectiveQuery))

		metadata := cloneStringMap(job.Metadata)
		metadata["daemon_job_id"] = job.JobID
		metadata["daemon_job_name"] = firstNonEmpty(job.Name, job.JobID)
		metadata["daemon_cycle_id"] = job.Status.ActiveRunID
		metadata["daemon_effective_query"] = effectiveQuery
		metadata["daemon_route_rank"] = fmt.Sprintf("%d", routed.Rank)
		metadata["daemon_route_score"] = fmt.Sprintf("%d", routed.Score)
		metadata["source_doc_path"] = routed.Doc.SourcePath

		record, err := d.server.executeRun(runRequest{
			DocID:          routed.Doc.DocID,
			Query:          effectiveQuery,
			TimeoutSeconds: job.TimeoutSeconds,
			Metadata:       metadata,
			PreludeSteps:   preludeSteps,
		})
		if err != nil {
			lastExecutionStatus = "error"
			if lastError == "" {
				lastError = err.Error()
			} else {
				lastError = lastError + "; " + err.Error()
			}
			continue
		}

		lastRecordIDs = append(lastRecordIDs, record.RecordID)
		lastExecutionStatus = mergeExecutionStatus(lastExecutionStatus, recordStatus(record))

		if job.AutoGenerateTree {
			docID, err := d.refreshGeneratedTree(routed.Doc, job)
			if err != nil {
				lastExecutionStatus = mergeExecutionStatus(lastExecutionStatus, "error")
				if lastError == "" {
					lastError = err.Error()
				} else {
					lastError = lastError + "; " + err.Error()
				}
			} else if docID != "" {
				lastGeneratedDocIDs = append(lastGeneratedDocIDs, docID)
			}
		}
	}

	if len(lastRecordIDs) == 0 && lastError == "" {
		lastExecutionStatus = "skipped"
		lastError = "no record generated in this cycle"
	}
}

func (d *daemonManager) finishJobRun(
	jobID string,
	startedAt time.Time,
	lastQuery string,
	lastProbeOutput string,
	lastRecordIDs []string,
	lastRoutedDocs []string,
	lastGeneratedDocIDs []string,
	lastExecutionStatus string,
	lastError string,
) {
	d.mu.Lock()
	defer d.mu.Unlock()

	runtime, ok := d.jobs[jobID]
	if !ok {
		return
	}

	runtime.running = false
	runtime.job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	runtime.job.Status.ActiveRunID = ""
	runtime.job.Status.LastRunAt = startedAt.Format(time.RFC3339)
	runtime.job.Status.LastQuery = lastQuery
	runtime.job.Status.LastProbeOutput = lastProbeOutput
	runtime.job.Status.LastRecordIDs = uniqueStrings(lastRecordIDs)
	runtime.job.Status.LastRoutedDocs = uniqueStrings(lastRoutedDocs)
	runtime.job.Status.LastGeneratedDocIDs = uniqueStrings(lastGeneratedDocIDs)
	runtime.job.Status.LastExecutionStatus = lastExecutionStatus
	runtime.job.Status.LastError = strings.TrimSpace(lastError)
	runtime.job.Status.RunCount++
	if runtime.job.Enabled {
		runtime.job.Status.State = "waiting"
		runtime.job.Status.NextRunAt = startedAt.Add(time.Duration(runtime.job.IntervalSeconds) * time.Second).Format(time.RFC3339)
	} else {
		runtime.job.Status.State = "stopped"
		runtime.job.Status.NextRunAt = ""
	}

	snapshot := runtime.job
	go saveDaemonJobToDB(d.server.db, snapshot)
}

func buildDaemonJob(req daemonJobUpsertRequest, existing *daemonJob) (daemonJob, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	job := daemonJob{}
	if existing != nil {
		job = *existing
	}

	if strings.TrimSpace(req.JobID) != "" {
		job.JobID = sanitize(req.JobID)
	}
	if strings.TrimSpace(job.JobID) == "" {
		base := firstNonEmpty(req.Name, req.Query)
		if base == "" && len(req.DocIDs) > 0 {
			base = strings.Join(req.DocIDs, "_")
		}
		if base == "" {
			base = fmt.Sprintf("daemon_job_%d", time.Now().UnixNano())
		}
		job.JobID = sanitize(base)
	}
	if strings.TrimSpace(req.Name) != "" {
		job.Name = strings.TrimSpace(req.Name)
	}
	if strings.TrimSpace(job.Name) == "" {
		job.Name = job.JobID
	}

	if req.Query != "" || existing == nil {
		job.Query = strings.TrimSpace(req.Query)
	}
	if req.DocIDs != nil || existing == nil {
		job.DocIDs = cleanStringList(req.DocIDs)
	}
	if req.Metadata != nil || existing == nil {
		job.Metadata = cloneStringMap(req.Metadata)
	}
	if req.ProbeAction != "" || req.ProbeText != "" || req.LegacyProbeCommand != "" || existing == nil {
		job.ProbeAction, job.ProbeText = normalizeProbeInput(req.ProbeAction, req.ProbeText, req.LegacyProbeCommand)
		job.LegacyProbeCommand = ""
	}
	if strings.TrimSpace(req.ProbeMergeMode) != "" || existing == nil {
		job.ProbeMergeMode = strings.ToLower(strings.TrimSpace(req.ProbeMergeMode))
	}

	if req.IntervalSeconds > 0 {
		job.IntervalSeconds = req.IntervalSeconds
	} else if job.IntervalSeconds <= 0 {
		job.IntervalSeconds = 300
	}
	if req.TimeoutSeconds > 0 {
		job.TimeoutSeconds = req.TimeoutSeconds
	} else if job.TimeoutSeconds <= 0 {
		job.TimeoutSeconds = 30
	}
	if req.MaxDocsPerCycle > 0 {
		job.MaxDocsPerCycle = req.MaxDocsPerCycle
	} else if job.MaxDocsPerCycle <= 0 {
		job.MaxDocsPerCycle = 3
	}
	if req.GenerationWindow > 0 {
		job.GenerationWindow = req.GenerationWindow
	} else if job.GenerationWindow <= 0 {
		job.GenerationWindow = 20
	}
	if req.MinRecordsToGenerate > 0 {
		job.MinRecordsToGenerate = req.MinRecordsToGenerate
	} else if job.MinRecordsToGenerate <= 0 {
		job.MinRecordsToGenerate = 3
	}

	if req.Enabled != nil {
		job.Enabled = *req.Enabled
	} else if existing == nil {
		job.Enabled = true
	}
	if req.AutoGenerateTree != nil {
		job.AutoGenerateTree = *req.AutoGenerateTree
	} else if existing == nil {
		job.AutoGenerateTree = true
	}
	if req.SkipIfProbeEmpty != nil {
		job.SkipIfProbeEmpty = *req.SkipIfProbeEmpty
	}

	if job.ProbeMergeMode != "replace" {
		job.ProbeMergeMode = "append"
	}
	if strings.TrimSpace(job.Query) == "" && len(job.DocIDs) == 0 && strings.TrimSpace(job.ProbeAction) == "" && strings.TrimSpace(job.ProbeText) == "" {
		return daemonJob{}, fmt.Errorf("query, doc_ids, probe_action, or probe_text is required")
	}

	if existing == nil {
		job.CreatedAt = now
		if job.Enabled {
			job.Status.State = "waiting"
			job.Status.NextRunAt = now
		} else {
			job.Status.State = "stopped"
		}
	} else {
		job.CreatedAt = existing.CreatedAt
		job.Status = existing.Status
		if job.Enabled {
			if job.Status.State == "" || job.Status.State == "stopped" {
				job.Status.State = "waiting"
			}
			if strings.TrimSpace(job.Status.NextRunAt) == "" {
				job.Status.NextRunAt = now
			}
		} else {
			job.Status.State = "stopped"
			job.Status.NextRunAt = ""
		}
	}
	job.UpdatedAt = now
	return job, nil
}

func normalizeLoadedDaemonJob(job daemonJob) daemonJob {
	if strings.TrimSpace(job.JobID) == "" {
		job.JobID = fmt.Sprintf("daemon_job_%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(job.Name) == "" {
		job.Name = job.JobID
	}
	if job.IntervalSeconds <= 0 {
		job.IntervalSeconds = 300
	}
	if job.TimeoutSeconds <= 0 {
		job.TimeoutSeconds = 30
	}
	if job.MaxDocsPerCycle <= 0 {
		job.MaxDocsPerCycle = 3
	}
	if job.GenerationWindow <= 0 {
		job.GenerationWindow = 20
	}
	if job.MinRecordsToGenerate <= 0 {
		job.MinRecordsToGenerate = 3
	}
	if strings.TrimSpace(job.ProbeMergeMode) == "" {
		job.ProbeMergeMode = "append"
	}
	job.ProbeAction, job.ProbeText = normalizeProbeInput(job.ProbeAction, job.ProbeText, job.LegacyProbeCommand)
	job.LegacyProbeCommand = ""
	if strings.TrimSpace(job.CreatedAt) == "" {
		job.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(job.UpdatedAt) == "" {
		job.UpdatedAt = job.CreatedAt
	}
	if job.Status.State == "running" || job.Status.State == "" {
		if job.Enabled {
			job.Status.State = "waiting"
			if strings.TrimSpace(job.Status.NextRunAt) == "" {
				job.Status.NextRunAt = time.Now().UTC().Format(time.RFC3339)
			}
		} else {
			job.Status.State = "stopped"
		}
		job.Status.ActiveRunID = ""
	}
	return job
}

func loadDaemonJobsFromDB(db *sql.DB) ([]daemonJob, error) {
	rows, err := db.Query(`
		SELECT payload_json
		  FROM daemon_jobs
		 ORDER BY updated_at DESC, job_id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []daemonJob{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var job daemonJob
		if err := json.Unmarshal([]byte(payload), &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func saveDaemonJobToDB(db *sql.DB, job daemonJob) error {
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	payload, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT OR REPLACE INTO daemon_jobs (job_id, created_at, updated_at, payload_json)
		 VALUES (?, ?, ?, ?)`,
		job.JobID,
		job.CreatedAt,
		job.UpdatedAt,
		string(payload),
	)
	return err
}

func buildProbePreludeStep(job daemonJob) (*experienceStep, string, string, bool, error) {
	effectiveQuery := strings.TrimSpace(job.Query)
	if strings.TrimSpace(job.ProbeAction) == "" && strings.TrimSpace(job.ProbeText) == "" {
		return nil, effectiveQuery, "", false, nil
	}

	runs := make([]ActionRun, 0, 2)
	if strings.TrimSpace(job.ProbeAction) != "" {
		run, err := executeActionBlock(job.ProbeAction, time.Duration(job.TimeoutSeconds)*time.Second)
		if err != nil {
			return nil, effectiveQuery, "", false, err
		}
		runs = append(runs, run)
	}
	if strings.TrimSpace(job.ProbeText) != "" {
		runs = append(runs, buildLiteralProbeActionRun(job.ProbeText))
	}
	if len(runs) == 0 {
		return nil, effectiveQuery, "", false, nil
	}

	outputParts := make([]string, 0, len(runs))
	for _, run := range runs {
		if strings.TrimSpace(run.Output) != "" {
			outputParts = append(outputParts, strings.TrimSpace(run.Output))
		}
	}
	probeOutput := strings.TrimSpace(strings.Join(outputParts, "\n"))
	effectiveQuery = mergeProbeQuery(job.Query, probeOutput, job.ProbeMergeMode)
	if effectiveQuery == "" {
		effectiveQuery = compactSignalText(probeOutput, 300)
	}
	if probeOutput == "" && job.SkipIfProbeEmpty {
		return nil, effectiveQuery, probeOutput, true, nil
	}

	status := "ok"
	for _, run := range runs {
		if actionRunFailed(run) {
			status = "failed"
			break
		}
	}

	return &experienceStep{
		NodeID: "daemon_probe",
		Title:  "步骤 0 探针信号",
		Path:   []string{"后台诊断", firstNonEmpty(job.Name, job.JobID), "探针"},
		Text: strings.TrimSpace(fmt.Sprintf(
			"任务: %s\n查询模板: %s\n有效查询: %s",
			job.JobID,
			firstNonEmpty(job.Query, "(empty)"),
			firstNonEmpty(effectiveQuery, "(empty)"),
		)),
		Actions: runs,
		Note:    limitText(probeOutput, 4000),
		Status:  status,
	}, effectiveQuery, probeOutput, false, nil
}

func decodeLegacyProbeText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, prefix := range []string{"printf", "echo"} {
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(text, prefix))
		rest = strings.TrimSuffix(rest, ";")
		if len(rest) >= 2 {
			if (strings.HasPrefix(rest, "\"") && strings.HasSuffix(rest, "\"")) ||
				(strings.HasPrefix(rest, "'") && strings.HasSuffix(rest, "'")) {
				return strings.TrimSpace(rest[1 : len(rest)-1])
			}
		}
		return strings.TrimSpace(rest)
	}
	return text
}

func resolveDocumentsForJob(db *sql.DB, docs []Document, job daemonJob, effectiveQuery string) ([]routedDocument, error) {
	maxDocs := job.MaxDocsPerCycle
	if maxDocs <= 0 {
		maxDocs = 3
	}

	byID := make(map[string]Document, len(docs))
	for _, doc := range docs {
		byID[doc.DocID] = doc
	}

	if len(job.DocIDs) > 0 {
		out := make([]routedDocument, 0, len(job.DocIDs))
		for _, docID := range cleanStringList(job.DocIDs) {
			doc, ok := byID[docID]
			if !ok || !isRunnableTroubleshootingTree(doc) {
				continue
			}
			out = append(out, routedDocument{
				Doc:   doc,
				Score: 1000 - len(out),
				Rank:  len(out) + 1,
			})
			if len(out) >= maxDocs {
				return out, nil
			}
		}
		return out, nil
	}

	candidates := make([]routedDocument, 0, maxDocs)
	query := strings.TrimSpace(effectiveQuery)
	if query == "" {
		for _, doc := range docs {
			if !isRunnableTroubleshootingTree(doc) {
				continue
			}
			candidates = append(candidates, routedDocument{Doc: doc, Score: 1})
		}
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Doc.DocID < candidates[j].Doc.DocID
		})
		if len(candidates) > maxDocs {
			candidates = candidates[:maxDocs]
		}
		for idx := range candidates {
			candidates[idx].Rank = idx + 1
		}
		return candidates, nil
	}

	hits, err := searchDocuments(db, docs, query, max(maxDocs*30, 100))
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, fmt.Errorf("no documents matched query %q", query)
	}

	docScores := map[string]int{}
	for _, hit := range hits {
		docScores[hit.DocID] += hit.Score
	}

	ordered := make([]routedDocument, 0, len(docScores))
	for _, doc := range docs {
		if docScores[doc.DocID] <= 0 || !isRunnableTroubleshootingTree(doc) {
			continue
		}
		ordered = append(ordered, routedDocument{
			Doc:   doc,
			Score: docScores[doc.DocID],
		})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Score == ordered[j].Score {
			return ordered[i].Doc.DocID < ordered[j].Doc.DocID
		}
		return ordered[i].Score > ordered[j].Score
	})
	if len(ordered) > maxDocs {
		ordered = ordered[:maxDocs]
	}
	for idx := range ordered {
		ordered[idx].Rank = idx + 1
	}
	return ordered, nil
}

func isRunnableTroubleshootingTree(doc Document) bool {
	if doc.SourceType == "experience" {
		return false
	}
	if doc.SourceType == "generated" {
		return false
	}
	if strings.HasPrefix(doc.DocID, "record_") {
		return false
	}
	return hasExecutableActions(extractStepViews(doc))
}

func buildRoutePreludeStep(job daemonJob, routed routedDocument, all []routedDocument, effectiveQuery string) experienceStep {
	lines := make([]string, 0, len(all)+3)
	lines = append(lines, fmt.Sprintf("任务: %s", job.JobID))
	lines = append(lines, fmt.Sprintf("有效查询: %s", firstNonEmpty(effectiveQuery, "(empty)")))
	lines = append(lines, "候选树:")
	for _, item := range all {
		lines = append(lines, fmt.Sprintf("- rank=%d score=%d doc_id=%s doc_name=%s", item.Rank, item.Score, item.Doc.DocID, item.Doc.DocName))
	}
	return experienceStep{
		NodeID: "daemon_route",
		Title:  "步骤 0 路由结果",
		Path:   []string{"后台诊断", firstNonEmpty(job.Name, job.JobID), "路由"},
		Text:   strings.Join(lines, "\n"),
		Note:   fmt.Sprintf("本次执行 rank=%d doc_id=%s", routed.Rank, routed.Doc.DocID),
		Status: "ok",
	}
}

func (d *daemonManager) refreshGeneratedTree(sourceDoc Document, job daemonJob) (string, error) {
	if sourceDoc.SourceType == "generated" || strings.HasPrefix(sourceDoc.DocID, "generated_") {
		return "", nil
	}
	records, err := exportExperienceRecordsBySource(d.server.db, sourceDoc.DocID, job.GenerationWindow)
	if err != nil {
		return "", err
	}
	if len(records) < job.MinRecordsToGenerate {
		return "", nil
	}

	markdown, err := buildGeneratedTreeMarkdown(sourceDoc, job, records)
	if err != nil {
		return "", err
	}

	docID := fmt.Sprintf("generated_%s", sanitize(sourceDoc.DocID))
	path := filepath.Join(d.generatedDir, docID+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(markdown), 0o644); err != nil {
		return "", err
	}

	doc, err := documentFromMarkdown(path)
	if err != nil {
		return "", err
	}
	doc.DocID = docID
	doc.DocName = fmt.Sprintf("自动生成树 %s", sourceDoc.DocName)
	doc.DocDescription = fmt.Sprintf("基于 %d 条经验自动生成，来源树 %s", len(records), sourceDoc.DocID)
	doc.SourceType = "generated"
	doc.SourcePath = path

	d.server.writeMu.Lock()
	defer d.server.writeMu.Unlock()
	if err := saveDocument(d.server.db, doc); err != nil {
		return "", err
	}
	if fp := fileFingerprint(path); fp != "" {
		if err := setIndexMeta(d.server.db, path, fp); err != nil {
			return "", err
		}
	}
	return doc.DocID, nil
}

func exportExperienceRecordsBySource(db *sql.DB, sourceDocID string, limit int) ([]experienceRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(
		`SELECT payload_json
		   FROM experience_records
		  WHERE source_doc_id = ?
		  ORDER BY updated_at DESC, record_id DESC
		  LIMIT ?`,
		sourceDocID,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []experienceRecord{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record experienceRecord
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, err
		}
		record = normalizeExperienceRecordShape(record)
		records = append(records, record)
	}
	return records, rows.Err()
}

func buildGeneratedTreeMarkdown(sourceDoc Document, job daemonJob, records []experienceRecord) (string, error) {
	groups := map[string]*generatedStepGroup{}
	statusCount := map[string]int{}
	jobNames := map[string]int{}
	latestUpdatedAt := ""

	for _, record := range records {
		record = normalizeExperienceRecordShape(record)
		statusCount[recordStatusFromSummary(record.Summary)]++
		if updated := firstNonEmpty(record.UpdatedAt, record.CreatedAt); updated > latestUpdatedAt {
			latestUpdatedAt = updated
		}
		if jobName := strings.TrimSpace(record.Metadata["daemon_job_name"]); jobName != "" {
			jobNames[jobName]++
		}
		for _, step := range record.Steps {
			step = normalizeExperienceStep(step)
			if strings.HasPrefix(step.NodeID, "daemon_") {
				continue
			}
			key := normalize(step.Title) + "|" + normalize(firstActionSpec(step.Actions))
			if strings.TrimSpace(key) == "|" {
				continue
			}
			group, ok := groups[key]
			if !ok {
				group = &generatedStepGroup{
					Title:        firstNonEmpty(step.Title, "步骤"),
					Text:         strings.TrimSpace(step.Text),
					ActionSpec:   firstActionSpec(step.Actions),
					OutputCounts: map[string]int{},
				}
				groups[key] = group
			}
			group.Total++
			if step.Status == "ok" {
				group.Succeeded++
			} else {
				group.Failed++
			}
			if updated := firstNonEmpty(record.UpdatedAt, record.CreatedAt); updated > group.LatestSeenAt {
				group.LatestSeenAt = updated
			}
			for _, line := range collectSignalLinesFromStep(step) {
				group.OutputCounts[line]++
			}
		}
	}

	stepGroups := make([]generatedStepGroup, 0, len(groups))
	for _, group := range groups {
		stepGroups = append(stepGroups, *group)
	}
	sort.Slice(stepGroups, func(i, j int) bool {
		leftFailure := float64(stepGroups[i].Failed) / float64(max(stepGroups[i].Total, 1))
		rightFailure := float64(stepGroups[j].Failed) / float64(max(stepGroups[j].Total, 1))
		if leftFailure == rightFailure {
			if stepGroups[i].Total == stepGroups[j].Total {
				return stepGroups[i].Title < stepGroups[j].Title
			}
			return stepGroups[i].Total > stepGroups[j].Total
		}
		return leftFailure > rightFailure
	})

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("# 自动生成树：%s\n\n", sourceDoc.DocName))
	builder.WriteString("## 步骤 1 经验概览\n")
	builder.WriteString(fmt.Sprintf("来源树: `%s`\n\n", sourceDoc.DocID))
	builder.WriteString(fmt.Sprintf("最近经验数: `%d`\n\n", len(records)))
	builder.WriteString(fmt.Sprintf("最新更新时间: `%s`\n\n", firstNonEmpty(latestUpdatedAt, time.Now().UTC().Format(time.RFC3339))))
	builder.WriteString("状态分布:\n")
	for _, key := range []string{"ok", "failed", "error", "skipped"} {
		if statusCount[key] == 0 {
			continue
		}
		builder.WriteString(fmt.Sprintf("- `%s`: %d\n", key, statusCount[key]))
	}
	if len(jobNames) > 0 {
		builder.WriteString("\n主要来源任务:\n")
		for _, item := range sortCountMap(jobNames) {
			builder.WriteString(fmt.Sprintf("- `%s`: %d\n", item.Key, item.Count))
		}
	}
	builder.WriteString("\n处理原则:\n")
	builder.WriteString("- 先跑失败率高的步骤。\n")
	builder.WriteString("- 对比最近成功输出和最近失败输出。\n")
	builder.WriteString("- 如果原树不够用，直接转到本自动树继续执行。\n\n")

	stepIndex := 2
	for _, group := range stepGroups {
		builder.WriteString(fmt.Sprintf("## 步骤 %d %s\n", stepIndex, group.Title))
		builder.WriteString(fmt.Sprintf("最近命中 `%d` 次，失败 `%d` 次，成功 `%d` 次。\n\n", group.Total, group.Failed, group.Succeeded))
		cleanText := cleanGeneratedStepText(group.Text)
		if strings.TrimSpace(cleanText) != "" {
			builder.WriteString("原始说明:\n")
			builder.WriteString(limitText(cleanText, 1500))
			builder.WriteString("\n\n")
		}
		if strings.TrimSpace(group.ActionSpec) != "" {
			builder.WriteString("推荐先执行这个内建动作确认当前状态:\n")
			builder.WriteString("```tsdiag\n")
			builder.WriteString(strings.TrimSpace(group.ActionSpec))
			builder.WriteString("\n```\n\n")
		}
		signals := topSignalLines(group.OutputCounts, 5)
		if len(signals) > 0 {
			builder.WriteString("近期高频现象:\n")
			for _, line := range signals {
				builder.WriteString(fmt.Sprintf("- %s\n", line))
			}
			builder.WriteString("\n")
		}
		builder.WriteString("处理建议:\n")
		for _, line := range deriveAdvice(signals) {
			builder.WriteString(fmt.Sprintf("- %s\n", line))
		}
		builder.WriteString("\n")
		stepIndex++
	}

	if len(stepGroups) == 0 {
		builder.WriteString("## 步骤 2 没有可聚合步骤\n")
		builder.WriteString("当前经验记录还不足以产出稳定的自动树，继续执行原始树积累经验。\n")
	}

	return builder.String(), nil
}

func recordStatus(record *experienceRecord) string {
	if record == nil {
		return "error"
	}
	return recordStatusFromSummary(record.Summary)
}

func recordStatusFromSummary(summary map[string]any) string {
	if summary == nil {
		return "ok"
	}
	if status, ok := summary["status"].(string); ok && strings.TrimSpace(status) != "" {
		return strings.TrimSpace(status)
	}
	return "ok"
}

func mergeExecutionStatus(current, next string) string {
	order := map[string]int{
		"ok":      0,
		"skipped": 1,
		"failed":  2,
		"error":   3,
	}
	if order[next] > order[current] {
		return next
	}
	return current
}

func mergeProbeQuery(baseQuery, probeOutput, mode string) string {
	baseQuery = strings.TrimSpace(baseQuery)
	probeSignal := compactSignalText(probeOutput, 300)
	if probeSignal == "" {
		return baseQuery
	}
	if mode == "replace" || baseQuery == "" {
		return probeSignal
	}
	if strings.Contains(baseQuery, probeSignal) {
		return baseQuery
	}
	return strings.TrimSpace(baseQuery + " " + probeSignal)
}

func compactSignalText(text string, limit int) string {
	parts := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts = append(parts, line)
		if len(parts) >= 6 {
			break
		}
	}
	return limitText(strings.Join(parts, " "), limit)
}

func limitText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len([]rune(text)) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit])) + "..."
}

func firstActionSpec(actions []ActionRun) string {
	for _, action := range actions {
		action = normalizeActionRun(action)
		if strings.TrimSpace(action.Spec) != "" {
			return strings.TrimSpace(action.Spec)
		}
	}
	return ""
}

func collectSignalLinesFromStep(step experienceStep) []string {
	step = normalizeExperienceStep(step)
	lines := []string{}
	if strings.TrimSpace(step.Note) != "" {
		for _, line := range extractSignalLines(step.Note, 2) {
			lines = append(lines, line)
		}
	}
	for _, action := range step.Actions {
		for _, line := range extractSignalLines(action.Output, 3) {
			lines = append(lines, line)
		}
	}
	return uniqueStrings(lines)
}

func extractSignalLines(text string, limit int) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	out := []string{}
	keywords := []string{"error", "fail", "failed", "refused", "denied", "timeout", "not found", "no such", "oom", "killed", "warn", "iowait", "unreachable"}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		normalized := normalize(line)
		for _, keyword := range keywords {
			if strings.Contains(normalized, keyword) {
				out = append(out, limitText(line, 160))
				break
			}
		}
		if len(out) >= limit {
			return uniqueStrings(out)
		}
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		out = append(out, limitText(line, 160))
		if len(out) >= limit {
			break
		}
	}
	return uniqueStrings(out)
}

func topSignalLines(counts map[string]int, top int) []string {
	type item struct {
		Line  string
		Count int
	}
	items := make([]item, 0, len(counts))
	for line, count := range counts {
		if strings.TrimSpace(line) == "" {
			continue
		}
		items = append(items, item{Line: line, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Line < items[j].Line
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > top {
		items = items[:top]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("%s (x%d)", item.Line, item.Count))
	}
	return out
}

func deriveAdvice(signals []string) []string {
	advice := []string{}
	joined := normalize(strings.Join(signals, " "))
	switch {
	case strings.Contains(joined, "refused") || strings.Contains(joined, "unreachable"):
		advice = append(advice, "优先检查端口监听、进程存活和网络连通性。")
	case strings.Contains(joined, "denied"):
		advice = append(advice, "优先检查执行权限、目录权限和 sudo 上下文。")
	case strings.Contains(joined, "no such") || strings.Contains(joined, "not found"):
		advice = append(advice, "优先检查文件路径、二进制路径和配置是否已经下发。")
	case strings.Contains(joined, "oom") || strings.Contains(joined, "killed"):
		advice = append(advice, "优先检查内存水位、OOM 记录和进程重启链路。")
	case strings.Contains(joined, "timeout") || strings.Contains(joined, "iowait"):
		advice = append(advice, "优先检查磁盘延迟、系统负载和依赖服务响应时间。")
	}
	advice = append(advice, "把本次输出和最近成功输出做对比，确认是环境问题还是流程分支变化。")
	advice = append(advice, "如果连续失败，回到原始树继续补充内建动作和判断条件。")
	return uniqueStrings(advice)
}

func cleanGeneratedStepText(text string) string {
	lines := []string{}
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

type countItem struct {
	Key   string
	Count int
}

func sortCountMap(input map[string]int) []countItem {
	items := make([]countItem, 0, len(input))
	for key, count := range input {
		items = append(items, countItem{Key: key, Count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return items[i].Key < items[j].Key
		}
		return items[i].Count > items[j].Count
	})
	return items
}

func parseRFC3339(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func cleanStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func uniqueStrings(values []string) []string {
	return cleanStringList(values)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
