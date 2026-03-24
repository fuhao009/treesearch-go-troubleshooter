package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

type apiServer struct {
	db           *sql.DB
	dbPath       string
	recordDir    string
	generatedDir string
	startedAt    time.Time
	writeMu      sync.Mutex
	daemon       *daemonManager
}

type documentMeta struct {
	DocID          string `json:"doc_id"`
	DocName        string `json:"doc_name"`
	DocDescription string `json:"doc_description"`
	SourceType     string `json:"source_type"`
	SourcePath     string `json:"source_path"`
}

type actionSource struct {
	Lang string `json:"lang"`
	Body string `json:"body"`
}

type stepView struct {
	Index   int            `json:"index"`
	NodeID  string         `json:"node_id"`
	Title   string         `json:"title"`
	Path    []string       `json:"path"`
	Text    string         `json:"text"`
	Actions []actionSource `json:"actions"`
}

type searchNodeView struct {
	NodeID    string   `json:"node_id"`
	Title     string   `json:"title"`
	Score     float64  `json:"score"`
	Text      string   `json:"text"`
	Ancestors []string `json:"ancestors,omitempty"`
}

type searchDocumentView struct {
	DocID   string           `json:"doc_id"`
	DocName string           `json:"doc_name"`
	Nodes   []searchNodeView `json:"nodes"`
}

type searchFlatNodeView struct {
	NodeID    string   `json:"node_id"`
	DocID     string   `json:"doc_id"`
	DocName   string   `json:"doc_name"`
	Title     string   `json:"title"`
	Score     float64  `json:"score"`
	Text      string   `json:"text"`
	Ancestors []string `json:"ancestors,omitempty"`
}

type experienceRecord struct {
	RecordID      string            `json:"record_id"`
	SourceDocID   string            `json:"source_doc_id"`
	SourceDocName string            `json:"source_doc_name"`
	Query         string            `json:"query,omitempty"`
	CreatedAt     string            `json:"created_at"`
	UpdatedAt     string            `json:"updated_at"`
	SourcePath    string            `json:"source_path,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Summary       map[string]any    `json:"summary"`
	Steps         []experienceStep  `json:"steps"`
}

type experienceStep struct {
	Index          int         `json:"index"`
	NodeID         string      `json:"node_id"`
	Title          string      `json:"title"`
	Path           []string    `json:"path"`
	Text           string      `json:"text"`
	Actions        []ActionRun `json:"actions,omitempty"`
	LegacyCommands []ActionRun `json:"commands,omitempty"`
	Note           string      `json:"note,omitempty"`
	Status         string      `json:"status"`
}

type playbooksImportRequest struct {
	Paths       []string `json:"paths"`
	Force       bool     `json:"force"`
	DropMissing bool     `json:"drop_missing,omitempty"`
}

type experienceImportRequest struct {
	Paths  []string        `json:"paths"`
	Record json.RawMessage `json:"record"`
}

type searchRequest struct {
	Query          string `json:"query"`
	TopKDocs       int    `json:"top_k_docs"`
	MaxNodesPerDoc int    `json:"max_nodes_per_doc"`
}

type executionResultRequest struct {
	Record json.RawMessage `json:"record"`
}

type runRequest struct {
	Query          string            `json:"query,omitempty"`
	DocID          string            `json:"doc_id,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	PreludeSteps   []experienceStep  `json:"-"`
}

func normalizeExperienceStep(step experienceStep) experienceStep {
	if len(step.Actions) == 0 && len(step.LegacyCommands) > 0 {
		step.Actions = append([]ActionRun(nil), step.LegacyCommands...)
	}
	for idx := range step.Actions {
		step.Actions[idx] = normalizeActionRun(step.Actions[idx])
	}
	step.LegacyCommands = nil
	return step
}

func normalizeExperienceRecordShape(record experienceRecord) experienceRecord {
	for idx := range record.Steps {
		record.Steps[idx] = normalizeExperienceStep(record.Steps[idx])
	}
	return record
}

type runResponse struct {
	RecordID string `json:"record_id,omitempty"`
	DocID    string `json:"doc_id,omitempty"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:19065", "listen address")
	dbPath := fs.String("db", filepath.Join("..", "indexes", "service.db"), "SQLite database path")
	recordDir := fs.String("record-dir", filepath.Join("..", "records"), "Directory to store execution records")
	generatedDir := fs.String("generated-dir", filepath.Join("..", "records", "generated_trees"), "Directory to store auto-generated troubleshooting trees")
	schedulerInterval := fs.Duration("scheduler-interval", 5*time.Second, "Background scheduler polling interval")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := os.MkdirAll(*recordDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(*generatedDir, 0o755); err != nil {
		return err
	}

	srv := &apiServer{
		db:           db,
		dbPath:       *dbPath,
		recordDir:    *recordDir,
		generatedDir: *generatedDir,
		startedAt:    time.Now().UTC(),
	}
	daemon, err := newDaemonManager(srv, *generatedDir, *schedulerInterval)
	if err != nil {
		return err
	}
	srv.daemon = daemon
	srv.daemon.Start()
	defer srv.daemon.Stop()

	engine := buildGinEngine(srv)

	log.Printf("go_walker all-in-one service listening on http://%s", *listen)
	server := &http.Server{
		Addr:    *listen,
		Handler: engine,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErrCh <- err
		}
		close(serveErrCh)
	}()

	shutdownSignalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErrCh:
		return err
	case <-shutdownSignalCtx.Done():
	}

	log.Printf("go_walker shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-serveErrCh
}

func buildGinEngine(srv *apiServer) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())

	engine.Any("/api/v1/health", gin.WrapF(srv.handleHealth))
	engine.Any("/api/v1/documents", gin.WrapF(srv.handleDocuments))
	engine.Any("/api/v1/playbooks/import", gin.WrapF(srv.handlePlaybookImport))
	engine.Any("/api/v1/experience/import", gin.WrapF(srv.handleExperienceImport))
	engine.Any("/api/v1/search", gin.WrapF(srv.handleSearch))
	engine.Any("/api/v1/doc/:doc_id", gin.WrapF(srv.handleDocument))
	engine.Any("/api/v1/executions/result", gin.WrapF(srv.handleExecutionResult))
	engine.Any("/api/v1/experience/export", gin.WrapF(srv.handleExperienceExport))
	engine.Any("/api/v1/run", gin.WrapF(srv.handleRun))
	engine.Any("/api/v1/daemon/status", gin.WrapF(srv.handleDaemonStatus))
	engine.Any("/api/v1/daemon/jobs", gin.WrapF(srv.handleDaemonJobs))
	engine.Any("/api/v1/daemon/jobs/:job_id", gin.WrapF(srv.handleDaemonJobAction))
	engine.Any("/api/v1/daemon/jobs/:job_id/:action", gin.WrapF(srv.handleDaemonJobAction))

	return engine
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	documentCount, err := countRows(s.db, "documents")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	experienceCount, err := countRows(s.db, "experience_records")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"component":        "go_walker_all_in_one",
		"db_path":          s.dbPath,
		"record_dir":       s.recordDir,
		"generated_dir":    s.generatedDir,
		"document_count":   documentCount,
		"experience_count": experienceCount,
		"runtime_started":  s.startedAt.Format(time.RFC3339),
		"daemon_status":    s.daemon.Status(),
	})
}

func (s *apiServer) handleDocuments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	docs, err := loadDocumentsFromDB(s.db)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	items := make([]documentMeta, 0, len(docs))
	for _, doc := range docs {
		items = append(items, toDocumentMeta(doc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": items})
}

func (s *apiServer) handlePlaybookImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	var req playbooksImportRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.Paths) == 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "paths is required")
		return
	}

	s.writeMu.Lock()
	imported, err := importSourceFiles(s.db, req.Paths, req.Force, req.DropMissing)
	s.writeMu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	items := make([]documentMeta, 0, len(imported))
	for _, doc := range imported {
		items = append(items, toDocumentMeta(doc))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported_count": len(imported),
		"documents":      items,
	})
}

func (s *apiServer) handleExperienceImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	var req experienceImportRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if len(req.Record) > 0 && string(req.Record) != "null" {
		var record experienceRecord
		if err := json.Unmarshal(req.Record, &record); err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		record = normalizeExperienceRecordShape(record)
		result, err := s.saveExperienceRecord(record, "")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	if len(req.Paths) == 0 {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "record or paths is required")
		return
	}

	expanded, err := expandSources(req.Paths)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	results := make([]map[string]any, 0, len(expanded))
	for _, path := range expanded {
		absPath, err := filepath.Abs(path)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		record, err := loadExperienceRecordFromFile(absPath)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		result, err := s.saveExperienceRecord(record, absPath)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		results = append(results, result)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"imported_count": len(results),
		"records":        results,
	})
}

func (s *apiServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	var req searchRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "query is required")
		return
	}
	if req.TopKDocs <= 0 {
		req.TopKDocs = 5
	}
	if req.MaxNodesPerDoc <= 0 {
		req.MaxNodesPerDoc = 5
	}

	docs, err := loadDocumentsFromDB(s.db)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	resp, err := buildSearchResponse(s.db, docs, query, req.TopKDocs, req.MaxNodesPerDoc)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *apiServer) handleDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	docID := strings.TrimPrefix(r.URL.Path, "/api/v1/doc/")
	decoded, err := url.PathUnescape(docID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	doc, err := loadDocumentByID(s.db, decoded)
	if err != nil {
		if err == sql.ErrNoRows {
			writeAPIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("document not found: %s", decoded))
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	steps := extractStepViews(doc)
	writeJSON(w, http.StatusOK, map[string]any{
		"doc_id":          doc.DocID,
		"doc_name":        doc.DocName,
		"doc_description": doc.DocDescription,
		"source_type":     doc.SourceType,
		"source_path":     doc.SourcePath,
		"structure":       doc.Structure,
		"steps":           steps,
	})
}

func (s *apiServer) handleExecutionResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	var req executionResultRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.Record) == 0 || string(req.Record) == "null" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "record is required")
		return
	}

	var record experienceRecord
	if err := json.Unmarshal(req.Record, &record); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	record = normalizeExperienceRecordShape(record)
	result, err := s.saveExperienceRecord(record, "")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *apiServer) handleExperienceExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	recordID := strings.TrimSpace(r.URL.Query().Get("record_id"))
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	if recordID != "" {
		record, err := exportExperienceRecord(s.db, recordID)
		if err != nil {
			if err == sql.ErrNoRows {
				writeAPIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("record not found: %s", recordID))
				return
			}
			writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"record": record})
		return
	}

	records, err := exportExperienceRecords(s.db, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

func (s *apiServer) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	var req runRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	record, err := s.executeRun(req)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	status := "ok"
	if summaryStatus, ok := record.Summary["status"].(string); ok && strings.TrimSpace(summaryStatus) != "" {
		status = summaryStatus
	}
	writeJSON(w, http.StatusOK, runResponse{
		RecordID: record.RecordID,
		DocID:    record.SourceDocID,
		Status:   fmt.Sprintf("%s:%d_steps", status, len(record.Steps)),
	})
}

func (s *apiServer) handleDaemonStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.daemon.Status())
}

func (s *apiServer) handleDaemonJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"jobs": s.daemon.ListJobs(),
		})
	case http.MethodPost:
		var req daemonJobUpsertRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		job, err := s.daemon.UpsertJob(req)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"job":    job,
			"status": "saved",
		})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
	}
}

func (s *apiServer) handleDaemonJobAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/daemon/jobs/")
	parts := cleanStringList(strings.Split(trimmed, "/"))
	if len(parts) == 0 {
		writeAPIError(w, http.StatusNotFound, "not_found", "job path is required")
		return
	}

	jobID := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
			return
		}
		job, ok := s.daemon.GetJob(jobID)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("job not found: %s", jobID))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
		return
	}

	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method_not_allowed")
		return
	}

	var (
		job daemonJob
		err error
	)
	switch parts[1] {
	case "start":
		job, err = s.daemon.StartJob(jobID)
	case "stop":
		job, err = s.daemon.StopJob(jobID)
	case "run":
		job, err = s.daemon.TriggerJob(jobID)
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unsupported action: %s", parts[1]))
		return
	}
	if err != nil {
		if err == sql.ErrNoRows {
			writeAPIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("job not found: %s", jobID))
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (s *apiServer) executeRun(req runRequest) (*experienceRecord, error) {
	docs, err := loadDocumentsFromDB(s.db)
	if err != nil {
		return nil, err
	}

	doc, err := resolveDocumentForRun(s.db, docs, strings.TrimSpace(req.DocID), strings.TrimSpace(req.Query))
	if err != nil {
		return nil, err
	}

	steps := extractStepViews(doc)
	if len(steps) == 0 {
		return nil, fmt.Errorf("document %s has no runnable steps", doc.DocID)
	}

	timeout := 30 * time.Second
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}

	now := time.Now().UTC().Format(time.RFC3339)
	totalSteps := len(steps) + len(req.PreludeSteps)
	record := &experienceRecord{
		RecordID:      fmt.Sprintf("record_%d", time.Now().UnixNano()),
		SourceDocID:   doc.DocID,
		SourceDocName: doc.DocName,
		Query:         strings.TrimSpace(req.Query),
		CreatedAt:     now,
		UpdatedAt:     now,
		Metadata:      req.Metadata,
		Summary:       map[string]any{},
		Steps:         make([]experienceStep, 0, totalSteps),
	}

	nextIndex := 1
	for _, prelude := range req.PreludeSteps {
		step := normalizeExperienceStep(prelude)
		step.Index = nextIndex
		if strings.TrimSpace(step.Status) == "" {
			step.Status = "ok"
		}
		record.Steps = append(record.Steps, step)
		nextIndex++
	}

	for _, step := range steps {
		outStep := experienceStep{
			Index:  nextIndex,
			NodeID: step.NodeID,
			Title:  step.Title,
			Path:   step.Path,
			Text:   step.Text,
			Status: "ok",
		}
		for _, block := range step.Actions {
			run, err := executeActionBlock(block.Body, timeout)
			if err != nil {
				outStep.Status = "error"
				outStep.Note = err.Error()
				continue
			}
			outStep.Actions = append(outStep.Actions, run)
			if actionRunFailed(run) {
				outStep.Status = "failed"
			}
		}
		record.Steps = append(record.Steps, outStep)
		nextIndex++
	}
	record.Summary = computeRecordSummary(record.Steps)

	if _, err := s.saveExperienceRecord(*record, ""); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *apiServer) saveExperienceRecord(record experienceRecord, sourcePath string) (map[string]any, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	record = normalizeExperienceRecordShape(record)
	record = normalizeExperienceRecord(record, sourcePath)

	if sourcePath == "" {
		if strings.TrimSpace(record.SourcePath) == "" {
			record.SourcePath = filepath.Join(s.recordDir, record.RecordID+".json")
		}
		if err := os.MkdirAll(filepath.Dir(record.SourcePath), 0o755); err != nil {
			return nil, err
		}
		payload, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(record.SourcePath, payload, 0o644); err != nil {
			return nil, err
		}
	}

	doc := documentFromExperienceRecord(record)
	if err := saveDocument(s.db, doc); err != nil {
		return nil, err
	}

	payloadJSON, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO experience_records
		 (record_id, source_doc_id, source_doc_name, created_at, updated_at, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		record.RecordID,
		record.SourceDocID,
		record.SourceDocName,
		record.CreatedAt,
		record.UpdatedAt,
		string(payloadJSON),
	); err != nil {
		return nil, err
	}

	return map[string]any{
		"record_id":   record.RecordID,
		"doc_id":      doc.DocID,
		"source_path": record.SourcePath,
	}, nil
}

func importSourceFiles(db *sql.DB, paths []string, force bool, dropMissing bool) ([]Document, error) {
	expanded, err := expandSources(paths)
	if err != nil {
		return nil, err
	}
	if len(expanded) == 0 {
		return nil, fmt.Errorf("no files matched the provided paths")
	}

	meta, err := getAllIndexMeta(db)
	if err != nil {
		return nil, err
	}

	seenSources := make(map[string]bool, len(expanded))
	imported := make([]Document, 0, len(expanded))
	for _, source := range expanded {
		absSource, err := filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		seenSources[absSource] = true

		fingerprint := fileFingerprint(absSource)
		if !force && meta[absSource] == fingerprint {
			continue
		}

		doc, err := documentFromFile(absSource)
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", absSource, err)
		}
		if err := saveDocument(db, doc); err != nil {
			return nil, err
		}
		if err := setIndexMeta(db, absSource, fingerprint); err != nil {
			return nil, err
		}
		imported = append(imported, doc)
	}

	if dropMissing {
		if err := removeMissingDocuments(db, seenSources); err != nil {
			return nil, err
		}
	}

	return imported, nil
}

func loadDocumentByID(db *sql.DB, docID string) (Document, error) {
	var doc Document
	var structureJSON string
	err := db.QueryRow(
		`SELECT doc_id, doc_name, doc_description, source_path, source_type, structure_json
		   FROM documents
		  WHERE doc_id = ?`,
		docID,
	).Scan(&doc.DocID, &doc.DocName, &doc.DocDescription, &doc.SourcePath, &doc.SourceType, &structureJSON)
	if err != nil {
		return Document{}, err
	}
	if strings.TrimSpace(structureJSON) != "" {
		if err := json.Unmarshal([]byte(structureJSON), &doc.Structure); err != nil {
			return Document{}, err
		}
	}
	return doc, nil
}

func extractStepViews(doc Document) []stepView {
	steps := extractStepNodes(doc)
	out := make([]stepView, 0, len(steps))
	for idx, step := range steps {
		blocks := extractActionBlocks(step.Text)
		actions := make([]actionSource, 0, len(blocks))
		for _, block := range blocks {
			actions = append(actions, actionSource{
				Lang: block.Lang,
				Body: block.Body,
			})
		}
		out = append(out, stepView{
			Index:   idx + 1,
			NodeID:  step.NodeID,
			Title:   step.Title,
			Path:    step.Path,
			Text:    step.Text,
			Actions: actions,
		})
	}
	return out
}

func buildSearchResponse(db *sql.DB, docs []Document, query string, topKDocs, maxNodesPerDoc int) (map[string]any, error) {
	limit := max(topKDocs*maxNodesPerDoc*10, 50)
	hits, err := searchDocuments(db, docs, query, limit)
	if err != nil {
		return nil, err
	}

	grouped := map[string][]SearchHit{}
	docScores := map[string]int{}
	for _, hit := range hits {
		docScores[hit.DocID] += hit.Score
		if len(grouped[hit.DocID]) < maxNodesPerDoc {
			grouped[hit.DocID] = append(grouped[hit.DocID], hit)
		}
	}

	docOrder := make([]string, 0, len(grouped))
	for docID := range grouped {
		docOrder = append(docOrder, docID)
	}
	sort.Slice(docOrder, func(i, j int) bool {
		if docScores[docOrder[i]] == docScores[docOrder[j]] {
			return docOrder[i] < docOrder[j]
		}
		return docScores[docOrder[i]] > docScores[docOrder[j]]
	})
	if len(docOrder) > topKDocs {
		docOrder = docOrder[:topKDocs]
	}

	docMeta := make(map[string]Document, len(docs))
	for _, doc := range docs {
		docMeta[doc.DocID] = doc
	}

	documents := make([]searchDocumentView, 0, len(docOrder))
	flatNodes := make([]searchFlatNodeView, 0, len(hits))
	for _, docID := range docOrder {
		doc := docMeta[docID]
		nodes := make([]searchNodeView, 0, len(grouped[docID]))
		for _, hit := range grouped[docID] {
			node := searchNodeView{
				NodeID:    hit.NodeID,
				Title:     hit.Title,
				Score:     float64(hit.Score),
				Text:      hit.Text,
				Ancestors: hit.Path[:max(len(hit.Path)-1, 0)],
			}
			nodes = append(nodes, node)
			flatNodes = append(flatNodes, searchFlatNodeView{
				NodeID:    hit.NodeID,
				DocID:     hit.DocID,
				DocName:   hit.DocName,
				Title:     hit.Title,
				Score:     float64(hit.Score),
				Text:      hit.Text,
				Ancestors: hit.Path[:max(len(hit.Path)-1, 0)],
			})
		}
		documents = append(documents, searchDocumentView{
			DocID:   docID,
			DocName: doc.DocName,
			Nodes:   nodes,
		})
	}

	return map[string]any{
		"documents":  documents,
		"query":      query,
		"flat_nodes": flatNodes,
	}, nil
}

func resolveDocumentForRun(db *sql.DB, docs []Document, docID, query string) (Document, error) {
	if docID != "" {
		for _, doc := range docs {
			if doc.DocID == docID {
				return doc, nil
			}
		}
		return Document{}, fmt.Errorf("document not found: %s", docID)
	}
	if query == "" {
		return Document{}, fmt.Errorf("query or doc_id is required")
	}

	hits, err := searchDocuments(db, docs, query, 100)
	if err != nil {
		return Document{}, err
	}
	if len(hits) == 0 {
		return Document{}, fmt.Errorf("no documents matched query %q", query)
	}

	docScores := map[string]int{}
	for _, hit := range hits {
		docScores[hit.DocID] += hit.Score
	}
	ordered := make([]Document, 0, len(docScores))
	for _, doc := range docs {
		if docScores[doc.DocID] > 0 {
			ordered = append(ordered, doc)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if docScores[ordered[i].DocID] == docScores[ordered[j].DocID] {
			return ordered[i].DocID < ordered[j].DocID
		}
		return docScores[ordered[i].DocID] > docScores[ordered[j].DocID]
	})
	if len(ordered) == 0 {
		return Document{}, fmt.Errorf("no documents matched query %q", query)
	}

	first := ordered[0]
	for _, doc := range ordered {
		if hasExecutableActions(extractStepViews(doc)) {
			return doc, nil
		}
	}
	return first, nil
}

func hasExecutableActions(steps []stepView) bool {
	for _, step := range steps {
		if len(step.Actions) > 0 {
			return true
		}
	}
	return false
}

func normalizeExperienceRecord(record experienceRecord, sourcePath string) experienceRecord {
	record = normalizeExperienceRecordShape(record)
	now := time.Now().UTC().Format(time.RFC3339)
	if strings.TrimSpace(record.RecordID) == "" {
		if strings.TrimSpace(sourcePath) != "" {
			record.RecordID = sanitize(strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath)))
		} else {
			record.RecordID = fmt.Sprintf("record_%d", time.Now().UnixNano())
		}
	}
	if strings.TrimSpace(record.CreatedAt) == "" {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if strings.TrimSpace(sourcePath) != "" {
		record.SourcePath = sourcePath
	}
	if strings.TrimSpace(record.SourceDocName) == "" {
		record.SourceDocName = record.SourceDocID
	}
	if record.Summary == nil {
		record.Summary = map[string]any{}
	}
	for key, value := range computeRecordSummary(record.Steps) {
		if _, ok := record.Summary[key]; !ok {
			record.Summary[key] = value
		}
	}
	return record
}

func computeRecordSummary(steps []experienceStep) map[string]any {
	status := "ok"
	executedActions := 0
	failedActions := 0
	for _, step := range steps {
		step = normalizeExperienceStep(step)
		if step.Status == "error" {
			status = "error"
		} else if step.Status == "failed" && status != "error" {
			status = "failed"
		}
		executedActions += len(step.Actions)
		for _, action := range step.Actions {
			if actionRunFailed(action) {
				failedActions++
				if status != "error" {
					status = "failed"
				}
			}
		}
	}
	return map[string]any{
		"status":           status,
		"total_steps":      len(steps),
		"executed_actions": executedActions,
		"failed_actions":   failedActions,
	}
}

func documentFromExperienceRecord(record experienceRecord) Document {
	record = normalizeExperienceRecordShape(record)
	rootPayload, _ := json.MarshalIndent(map[string]any{
		"record_id":  record.RecordID,
		"query":      record.Query,
		"created_at": record.CreatedAt,
		"updated_at": record.UpdatedAt,
		"summary":    record.Summary,
		"metadata":   record.Metadata,
	}, "", "  ")

	root := Node{
		NodeID: "0",
		Title:  fmt.Sprintf("排查记录：%s", firstNonEmpty(record.SourceDocName, record.RecordID)),
		Text:   string(rootPayload),
	}
	children := make([]Node, 0, len(record.Steps))
	for idx, step := range record.Steps {
		title := strings.TrimSpace(step.Title)
		if title == "" {
			title = fmt.Sprintf("步骤 %d", idx+1)
		}
		children = append(children, Node{
			NodeID: fmt.Sprintf("%d", idx+1),
			Title:  title,
			Text:   buildExperienceStepText(step),
		})
	}
	root.Nodes = children

	return Document{
		DocID:          record.RecordID,
		DocName:        record.RecordID,
		DocDescription: fmt.Sprintf("排查记录 %s，步骤数 %d", firstNonEmpty(record.SourceDocName, record.SourceDocID), len(record.Steps)),
		SourcePath:     record.SourcePath,
		SourceType:     "experience",
		Structure:      []Node{root},
	}
}

func buildExperienceStepText(step experienceStep) string {
	step = normalizeExperienceStep(step)
	var builder strings.Builder
	builder.WriteString(strings.TrimSpace(step.Text))
	if strings.TrimSpace(step.Note) != "" {
		builder.WriteString("\n\n观察记录\n")
		builder.WriteString(strings.TrimSpace(step.Note))
	}
	for idx, action := range step.Actions {
		builder.WriteString(fmt.Sprintf("\n\n动作块 %d\n", idx+1))
		builder.WriteString(action.Spec)
		builder.WriteString(fmt.Sprintf("\nexit_code=%d", action.ExitCode))
		if action.TimedOut {
			builder.WriteString("\ntimed_out=true")
		}
		if strings.TrimSpace(action.Output) != "" {
			builder.WriteString("\noutput:\n")
			builder.WriteString(strings.TrimSpace(action.Output))
		}
	}
	return strings.TrimSpace(builder.String())
}

func loadExperienceRecordFromFile(path string) (experienceRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return experienceRecord{}, err
	}

	var record experienceRecord
	if err := json.Unmarshal(data, &record); err == nil && looksLikeExperienceRecord(record) {
		record = normalizeExperienceRecordShape(record)
		record.SourcePath = path
		return record, nil
	}

	var walk WalkRecord
	if err := json.Unmarshal(data, &walk); err == nil && looksLikeWalkRecord(walk) {
		walk = normalizeWalkRecord(walk)
		return experienceRecordFromWalkRecord(walk, path), nil
	}

	return experienceRecord{}, fmt.Errorf("unsupported experience payload: %s", path)
}

func looksLikeExperienceRecord(record experienceRecord) bool {
	record = normalizeExperienceRecordShape(record)
	return strings.TrimSpace(record.RecordID) != "" ||
		strings.TrimSpace(record.SourceDocID) != "" ||
		strings.TrimSpace(record.SourceDocName) != "" ||
		len(record.Steps) > 0 ||
		len(record.Summary) > 0
}

func looksLikeWalkRecord(record WalkRecord) bool {
	record = normalizeWalkRecord(record)
	return strings.TrimSpace(record.DocID) != "" ||
		strings.TrimSpace(record.DocName) != "" ||
		len(record.Steps) > 0
}

func experienceRecordFromWalkRecord(record WalkRecord, sourcePath string) experienceRecord {
	record = normalizeWalkRecord(record)
	steps := make([]experienceStep, 0, len(record.Steps))
	for _, step := range record.Steps {
		status := "ok"
		for _, action := range step.Actions {
			if actionRunFailed(action) {
				status = "failed"
				break
			}
		}
		steps = append(steps, experienceStep{
			Index:   step.Index,
			NodeID:  step.NodeID,
			Title:   step.Title,
			Path:    step.Path,
			Text:    step.Text,
			Actions: step.Actions,
			Note:    step.Note,
			Status:  status,
		})
	}

	recordID := sanitize(strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath)))
	if recordID == "" {
		recordID = fmt.Sprintf("record_%d", time.Now().UnixNano())
	}

	return experienceRecord{
		RecordID:      recordID,
		SourceDocID:   record.DocID,
		SourceDocName: record.DocName,
		Query:         record.Query,
		CreatedAt:     record.GeneratedAt,
		UpdatedAt:     record.GeneratedAt,
		SourcePath:    sourcePath,
		Summary:       computeRecordSummary(steps),
		Steps:         steps,
	}
}

func exportExperienceRecord(db *sql.DB, recordID string) (experienceRecord, error) {
	var payload string
	err := db.QueryRow(
		`SELECT payload_json FROM experience_records WHERE record_id = ?`,
		recordID,
	).Scan(&payload)
	if err != nil {
		return experienceRecord{}, err
	}
	var record experienceRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return experienceRecord{}, err
	}
	record = normalizeExperienceRecordShape(record)
	return record, nil
}

func exportExperienceRecords(db *sql.DB, limit int) ([]experienceRecord, error) {
	rows, err := db.Query(
		`SELECT payload_json
		   FROM experience_records
		  ORDER BY updated_at DESC, record_id DESC
		  LIMIT ?`,
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

func countRows(db *sql.DB, table string) (int, error) {
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", table)
	if err := db.QueryRow(query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func toDocumentMeta(doc Document) documentMeta {
	return documentMeta{
		DocID:          doc.DocID,
		DocName:        doc.DocName,
		DocDescription: doc.DocDescription,
		SourceType:     doc.SourceType,
		SourcePath:     doc.SourcePath,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func decodeJSONBody(r *http.Request, out any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": message,
	})
}
