package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type runRequest struct {
	Query          string            `json:"query,omitempty"`
	DocID          string            `json:"doc_id,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Shell          string            `json:"shell,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type runResponse struct {
	RecordID string `json:"record_id,omitempty"`
	DocID    string `json:"doc_id,omitempty"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

type searchResponse struct {
	Documents []struct {
		DocID string `json:"doc_id"`
	} `json:"documents"`
	FlatNodes []struct {
		DocID string  `json:"doc_id"`
		Score float64 `json:"score"`
	} `json:"flat_nodes"`
}

type documentResponse struct {
	DocID   string     `json:"doc_id"`
	DocName string     `json:"doc_name"`
	Steps   []stepView `json:"steps"`
}

type stepView struct {
	Index    int         `json:"index"`
	NodeID   string      `json:"node_id"`
	Title    string      `json:"title"`
	Path     []string    `json:"path"`
	Text     string      `json:"text"`
	Commands []cmdSource `json:"commands"`
}

type cmdSource struct {
	Lang string `json:"lang"`
	Body string `json:"body"`
}

type recordPayload struct {
	RecordID      string            `json:"record_id"`
	SourceDocID   string            `json:"source_doc_id"`
	SourceDocName string            `json:"source_doc_name"`
	Query         string            `json:"query,omitempty"`
	CreatedAt     string            `json:"created_at"`
	UpdatedAt     string            `json:"updated_at"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Summary       map[string]any    `json:"summary"`
	Steps         []recordStep      `json:"steps"`
}

type recordStep struct {
	Index    int          `json:"index"`
	NodeID   string       `json:"node_id"`
	Title    string       `json:"title"`
	Path     []string     `json:"path"`
	Text     string       `json:"text"`
	Commands []commandRun `json:"commands,omitempty"`
	Note     string       `json:"note,omitempty"`
	Status   string       `json:"status"`
}

type commandRun struct {
	Shell    string `json:"shell"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	RanAt    string `json:"ran_at"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

type executorServer struct {
	treeSearchURL string
	client        *http.Client
	startedAt     time.Time
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8788", "listen address")
	treeSearchURL := flag.String("treesearch", "http://127.0.0.1:8765", "TreeSearch service URL")
	flag.Parse()

	srv := &executorServer{
		treeSearchURL: strings.TrimRight(*treeSearchURL, "/"),
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		startedAt: time.Now().UTC(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", srv.handleHealth)
	mux.HandleFunc("/api/v1/run", srv.handleRun)

	log.Printf("executor unit listening on http://%s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func (s *executorServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"treesearch_url":  s.treeSearchURL,
		"component":       "executor_unit",
		"runtime_started": s.startedAt.Format(time.RFC3339),
	})
}

func (s *executorServer) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	record, err := s.execute(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, runResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, runResponse{
		RecordID: record.RecordID,
		DocID:    record.SourceDocID,
		Status:   fmt.Sprintf("ok:%d_steps", len(record.Steps)),
	})
}

func (s *executorServer) execute(req runRequest) (*recordPayload, error) {
	docID := strings.TrimSpace(req.DocID)
	if docID == "" {
		if strings.TrimSpace(req.Query) == "" {
			return nil, fmt.Errorf("query or doc_id is required")
		}
		resolved, err := s.resolveDocID(req.Query)
		if err != nil {
			return nil, err
		}
		docID = resolved
	}

	doc, err := s.fetchDocument(docID)
	if err != nil {
		return nil, err
	}

	shell := req.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	recordID := fmt.Sprintf("record_%d", time.Now().UnixNano())
	now := time.Now().UTC().Format(time.RFC3339)
	record := &recordPayload{
		RecordID:      recordID,
		SourceDocID:   doc.DocID,
		SourceDocName: doc.DocName,
		Query:         req.Query,
		CreatedAt:     now,
		UpdatedAt:     now,
		Metadata:      req.Metadata,
		Summary: map[string]any{
			"status":            "ok",
			"total_steps":       len(doc.Steps),
			"executed_commands": 0,
			"failed_commands":   0,
		},
		Steps: make([]recordStep, 0, len(doc.Steps)),
	}

	for _, step := range doc.Steps {
		outStep := recordStep{
			Index:  step.Index,
			NodeID: step.NodeID,
			Title:  step.Title,
			Path:   step.Path,
			Text:   step.Text,
			Status: "ok",
		}
		for _, command := range step.Commands {
			run, err := runCommand(shell, command.Body, timeout)
			if err != nil {
				outStep.Status = "error"
				outStep.Note = err.Error()
				record.Summary["status"] = "error"
				record.Summary["failed_commands"] = record.Summary["failed_commands"].(int) + 1
				continue
			}
			outStep.Commands = append(outStep.Commands, run)
			record.Summary["executed_commands"] = record.Summary["executed_commands"].(int) + 1
			if run.ExitCode != 0 || run.TimedOut {
				outStep.Status = "failed"
				record.Summary["status"] = "failed"
				record.Summary["failed_commands"] = record.Summary["failed_commands"].(int) + 1
			}
		}
		record.Steps = append(record.Steps, outStep)
	}

	if err := s.pushResult(record); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *executorServer) resolveDocID(query string) (string, error) {
	body := map[string]any{
		"query":             query,
		"top_k_docs":        5,
		"max_nodes_per_doc": 5,
	}
	var resp searchResponse
	if err := s.postJSON("/api/v1/search", body, &resp); err != nil {
		return "", err
	}

	docScores := map[string]float64{}
	for _, hit := range resp.FlatNodes {
		docScores[hit.DocID] += hit.Score
	}
	candidates := make([]string, 0, len(resp.Documents)+len(docScores))
	type scoredDoc struct {
		docID string
		score float64
	}
	ranked := make([]scoredDoc, 0, len(docScores))
	for docID, score := range docScores {
		ranked = append(ranked, scoredDoc{docID: docID, score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].docID < ranked[j].docID
		}
		return ranked[i].score > ranked[j].score
	})
	seen := map[string]bool{}
	for _, item := range ranked {
		if !seen[item.docID] {
			candidates = append(candidates, item.docID)
			seen[item.docID] = true
		}
	}
	for _, doc := range resp.Documents {
		if !seen[doc.DocID] {
			candidates = append(candidates, doc.DocID)
			seen[doc.DocID] = true
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no document matched query: %s", query)
	}

	firstDoc := candidates[0]
	for _, docID := range candidates {
		doc, err := s.fetchDocument(docID)
		if err != nil {
			continue
		}
		if hasExecutableCommands(doc) {
			return docID, nil
		}
	}
	return firstDoc, nil
}

func (s *executorServer) fetchDocument(docID string) (*documentResponse, error) {
	req, err := http.NewRequest(http.MethodGet, s.treeSearchURL+"/api/v1/doc/"+url.PathEscape(docID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch document failed: %s", string(body))
	}
	var out documentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func hasExecutableCommands(doc *documentResponse) bool {
	for _, step := range doc.Steps {
		if len(step.Commands) > 0 {
			return true
		}
	}
	return false
}

func (s *executorServer) pushResult(record *recordPayload) error {
	body := map[string]any{"record": record}
	var resp map[string]any
	return s.postJSON("/api/v1/executions/result", body, &resp)
}

func (s *executorServer) postJSON(path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := s.client.Post(s.treeSearchURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed: %s", string(payload))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func runCommand(shell, command string, timeout time.Duration) (commandRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	script := "set -e\n(set -o pipefail) 2>/dev/null && set -o pipefail\n" + command
	cmd := exec.CommandContext(ctx, shell, "-c", script)
	output, err := cmd.CombinedOutput()
	run := commandRun{
		Shell:    shell,
		Command:  command,
		Output:   string(output),
		RanAt:    time.Now().UTC().Format(time.RFC3339),
		ExitCode: 0,
		TimedOut: ctx.Err() == context.DeadlineExceeded,
	}
	if err == nil {
		return run, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		run.ExitCode = exitErr.ExitCode()
		return run, nil
	}
	return run, err
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
