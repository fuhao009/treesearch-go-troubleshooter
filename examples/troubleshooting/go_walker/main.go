package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var stepTitlePattern = regexp.MustCompile(`(?i)^(?:step|步骤)\s*[0-9一二三四五六七八九十]+`)
var markdownHeadingPattern = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
var codeFencePattern = regexp.MustCompile("(?s)```([a-zA-Z0-9_-]*)\\n(.*?)```")

const schemaSQL = `
CREATE TABLE IF NOT EXISTS documents (
	doc_id TEXT PRIMARY KEY,
	doc_name TEXT DEFAULT '',
	doc_description TEXT DEFAULT '',
	source_path TEXT DEFAULT '',
	source_type TEXT DEFAULT '',
	structure_json TEXT DEFAULT '',
	node_count INTEGER DEFAULT 0,
	index_hash TEXT
);

CREATE TABLE IF NOT EXISTS nodes (
	node_id TEXT NOT NULL,
	doc_id TEXT NOT NULL,
	title TEXT DEFAULT '',
	summary TEXT DEFAULT '',
	depth INTEGER DEFAULT 0,
	line_start INTEGER,
	line_end INTEGER,
	parent_node_id TEXT,
	content_hash TEXT,
	PRIMARY KEY (doc_id, node_id)
);

CREATE TABLE IF NOT EXISTS index_meta (
	source_path TEXT PRIMARY KEY,
	file_hash TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS experience_records (
	record_id TEXT PRIMARY KEY,
	source_doc_id TEXT DEFAULT '',
	source_doc_name TEXT DEFAULT '',
	created_at TEXT DEFAULT '',
	updated_at TEXT DEFAULT '',
	payload_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS daemon_jobs (
	job_id TEXT PRIMARY KEY,
	created_at TEXT DEFAULT '',
	updated_at TEXT DEFAULT '',
	payload_json TEXT NOT NULL
);
`

type Node struct {
	NodeID        string `json:"node_id"`
	Title         string `json:"title"`
	Summary       string `json:"summary,omitempty"`
	PrefixSummary string `json:"prefix_summary,omitempty"`
	Text          string `json:"text,omitempty"`
	Nodes         []Node `json:"nodes,omitempty"`
}

type Document struct {
	DocID          string
	DocName        string
	DocDescription string
	SourcePath     string
	SourceType     string
	Structure      []Node
}

type FlatNode struct {
	DocID   string
	DocName string
	NodeID  string
	Title   string
	Text    string
	Path    []string
}

type SearchHit struct {
	DocID   string
	DocName string
	NodeID  string
	Title   string
	Text    string
	Path    []string
	Score   int
}

type ActionRun struct {
	Engine        string `json:"engine,omitempty"`
	Spec          string `json:"spec,omitempty"`
	ExitCode      int    `json:"exit_code"`
	Output        string `json:"output"`
	RanAt         string `json:"ran_at"`
	TimedOut      bool   `json:"timed_out,omitempty"`
	LegacyShell   string `json:"shell,omitempty"`
	LegacyCommand string `json:"command,omitempty"`
}

type WalkStep struct {
	Index          int         `json:"index"`
	NodeID         string      `json:"node_id"`
	Title          string      `json:"title"`
	Path           []string    `json:"path"`
	Text           string      `json:"text"`
	Note           string      `json:"note,omitempty"`
	Actions        []ActionRun `json:"actions,omitempty"`
	LegacyCommands []ActionRun `json:"commands,omitempty"`
	ViewedAt       string      `json:"viewed_at"`
}

type WalkRecord struct {
	GeneratedAt string     `json:"generated_at"`
	DBPath      string     `json:"db_path"`
	Query       string     `json:"query,omitempty"`
	DocID       string     `json:"doc_id"`
	DocName     string     `json:"doc_name"`
	SourcePath  string     `json:"source_path,omitempty"`
	Steps       []WalkStep `json:"steps"`
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	*f = append(*f, value)
	return nil
}

type markdownMarker struct {
	Title string
	Line  int
	Level int
}

type flatMarkdownNode struct {
	Title string
	Level int
	Text  string
}

type actionBlock struct {
	Lang string
	Body string
}

func normalizeActionRun(run ActionRun) ActionRun {
	run.Engine = firstNonEmpty(strings.TrimSpace(run.Engine), strings.TrimSpace(run.LegacyShell), "go_builtin")
	run.Spec = firstNonEmpty(strings.TrimSpace(run.Spec), strings.TrimSpace(run.LegacyCommand))
	run.LegacyShell = ""
	run.LegacyCommand = ""
	return run
}

func normalizeWalkStep(step WalkStep) WalkStep {
	if len(step.Actions) == 0 && len(step.LegacyCommands) > 0 {
		step.Actions = append([]ActionRun(nil), step.LegacyCommands...)
	}
	for idx := range step.Actions {
		step.Actions[idx] = normalizeActionRun(step.Actions[idx])
	}
	step.LegacyCommands = nil
	return step
}

func normalizeWalkRecord(record WalkRecord) WalkRecord {
	for idx := range record.Steps {
		record.Steps[idx] = normalizeWalkStep(record.Steps[idx])
	}
	return record
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve failed:", err)
			os.Exit(1)
		}
	case "index":
		if err := runIndex(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "index failed:", err)
			os.Exit(1)
		}
	case "list":
		if err := runList(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "list failed:", err)
			os.Exit(1)
		}
	case "search":
		if err := runSearch(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "search failed:", err)
			os.Exit(1)
		}
	case "walk":
		if err := runWalk(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "walk failed:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("Usage:")
	fmt.Println("  go_walker serve  --listen <host:port> --db <db> --record-dir <dir>")
	fmt.Println("  go_walker index  --db <db> --source <glob> [--source <glob> ...] [--force]")
	fmt.Println("  go_walker list   --db <db>")
	fmt.Println("  go_walker search --db <db> --query <query> [--top 10]")
	fmt.Println("  go_walker walk   --db <db> (--doc <doc_id> | --query <query>) [--record <path>] [--exec]")
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite database path")
	force := fs.Bool("force", false, "Re-import even if source fingerprint is unchanged")
	dropMissing := fs.Bool("drop-missing", false, "Delete documents whose source files are no longer matched")
	var sources stringListFlag
	fs.Var(&sources, "source", "Source glob or file path. Repeatable.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if extras := fs.Args(); len(extras) > 0 {
		sources = append(sources, extras...)
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}
	if len(sources) == 0 {
		return fmt.Errorf("at least one --source is required")
	}

	expanded, err := expandSources(sources)
	if err != nil {
		return err
	}
	if len(expanded) == 0 {
		return fmt.Errorf("no files matched the provided --source patterns")
	}

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	meta, err := getAllIndexMeta(db)
	if err != nil {
		return err
	}

	seenSources := make(map[string]bool, len(expanded))
	imported := 0
	skipped := 0
	for _, source := range expanded {
		absSource, err := filepath.Abs(source)
		if err != nil {
			return err
		}
		seenSources[absSource] = true

		fingerprint := fileFingerprint(absSource)
		if !*force && meta[absSource] == fingerprint {
			skipped++
			continue
		}

		doc, err := documentFromFile(absSource)
		if err != nil {
			return fmt.Errorf("import %s: %w", absSource, err)
		}
		if err := saveDocument(db, doc); err != nil {
			return err
		}
		if err := setIndexMeta(db, absSource, fingerprint); err != nil {
			return err
		}
		imported++
		fmt.Printf("Imported: %s -> %s\n", absSource, doc.DocID)
	}

	if *dropMissing {
		if err := removeMissingDocuments(db, seenSources); err != nil {
			return err
		}
	}

	fmt.Printf("Index complete. imported=%d skipped=%d db=%s\n", imported, skipped, *dbPath)
	return nil
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}

	docs, err := loadDocuments(*dbPath)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		fmt.Println("No documents found.")
		return nil
	}

	fmt.Printf("Documents in %s\n\n", *dbPath)
	for _, doc := range docs {
		stepCount := len(extractStepNodes(doc))
		fmt.Printf("- %s (%s)\n", doc.DocID, doc.DocName)
		if doc.SourceType != "" {
			fmt.Printf("  type: %s\n", doc.SourceType)
		}
		if doc.SourcePath != "" {
			fmt.Printf("  source: %s\n", doc.SourcePath)
		}
		if doc.DocDescription != "" {
			fmt.Printf("  desc: %s\n", trimForDisplay(doc.DocDescription, 140))
		}
		fmt.Printf("  steps: %d\n", stepCount)
	}
	return nil
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite database path")
	query := fs.String("query", "", "Search query")
	top := fs.Int("top", 10, "Top hits to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}
	if strings.TrimSpace(*query) == "" {
		return fmt.Errorf("--query is required")
	}

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	docs, err := loadDocuments(*dbPath)
	if err != nil {
		return err
	}
	hits, err := searchDocuments(db, docs, *query, *top)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("No hits.")
		return nil
	}

	fmt.Printf("Top %d hits for %q\n\n", min(*top, len(hits)), *query)
	for idx, hit := range hits {
		fmt.Printf("%d. [%d] %s / %s\n", idx+1, hit.Score, hit.DocName, hit.Title)
		if len(hit.Path) > 0 {
			fmt.Printf("   path: %s\n", strings.Join(hit.Path, " > "))
		}
		fmt.Printf("   node_id: %s\n", hit.NodeID)
		fmt.Printf("   text: %s\n", trimForDisplay(hit.Text, 180))
	}
	return nil
}

func runWalk(args []string) error {
	fs := flag.NewFlagSet("walk", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite database path")
	docID := fs.String("doc", "", "Document ID to walk")
	query := fs.String("query", "", "Pick the best matching document by query")
	recordPath := fs.String("record", "", "Path to save the step-by-step JSON record")
	execEnabled := fs.Bool("exec", false, "Execute fenced tsdiag action blocks found in each step")
	timeout := fs.Duration("timeout", 30*time.Second, "Timeout per tsdiag action block")
	learn := fs.Bool("learn", true, "Write the generated record back into the SQLite knowledge base")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}
	if strings.TrimSpace(*docID) == "" && strings.TrimSpace(*query) == "" {
		return fmt.Errorf("either --doc or --query is required")
	}

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	docs, err := loadDocuments(*dbPath)
	if err != nil {
		return err
	}
	doc, selectedQuery, err := pickDocument(db, docs, strings.TrimSpace(*docID), strings.TrimSpace(*query))
	if err != nil {
		return err
	}

	steps := extractStepNodes(doc)
	if len(steps) == 0 {
		return fmt.Errorf("document %s has no step nodes matching '步骤 N' or 'Step N'", doc.DocID)
	}

	if strings.TrimSpace(*recordPath) == "" {
		*recordPath = defaultRecordPath(doc.DocID)
	}

	fmt.Printf("Walking playbook: %s (%s)\n", doc.DocName, doc.DocID)
	if doc.SourcePath != "" {
		fmt.Printf("Source: %s\n", doc.SourcePath)
	}
	fmt.Printf("Steps: %d\n", len(steps))
	if selectedQuery != "" {
		fmt.Printf("Selected by query: %q\n", selectedQuery)
	}
	fmt.Println()

	record := WalkRecord{
		GeneratedAt: time.Now().Format(time.RFC3339),
		DBPath:      *dbPath,
		Query:       selectedQuery,
		DocID:       doc.DocID,
		DocName:     doc.DocName,
		SourcePath:  doc.SourcePath,
		Steps:       make([]WalkStep, 0, len(steps)),
	}

	reader := bufio.NewReader(os.Stdin)
	for idx, step := range steps {
		fmt.Printf("===== Step %d/%d =====\n", idx+1, len(steps))
		fmt.Println(step.Title)
		if len(step.Path) > 0 {
			fmt.Printf("Path: %s\n", strings.Join(step.Path, " > "))
		}
		fmt.Printf("Node: %s\n\n", step.NodeID)
		fmt.Println(strings.TrimSpace(step.Text))
		fmt.Println()

		actionRuns := []ActionRun{}
		if *execEnabled {
			blocks := extractActionBlocks(step.Text)
			for blockIdx, block := range blocks {
				fmt.Printf("Builtin action block %d detected (%s).\n", blockIdx+1, block.Lang)
				fmt.Print("Run this action? [y/N]: ")
				answer, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				if !strings.EqualFold(strings.TrimSpace(answer), "y") {
					continue
				}

				run, err := executeActionBlock(block.Body, *timeout)
				if err != nil {
					return err
				}
				actionRuns = append(actionRuns, run)

				fmt.Printf("Exit code: %d\n", run.ExitCode)
				if run.TimedOut {
					fmt.Println("Timed out: true")
				}
				if strings.TrimSpace(run.Output) != "" {
					fmt.Printf("Output:\n%s\n", run.Output)
				}
			}
		}

		fmt.Print("Observation for this step (Enter to skip, q to stop): ")
		note, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		note = strings.TrimSpace(note)
		if strings.EqualFold(note, "q") {
			break
		}

		record.Steps = append(record.Steps, WalkStep{
			Index:    idx + 1,
			NodeID:   step.NodeID,
			Title:    step.Title,
			Path:     step.Path,
			Text:     step.Text,
			Note:     note,
			Actions:  actionRuns,
			ViewedAt: time.Now().Format(time.RFC3339),
		})

		if idx < len(steps)-1 {
			fmt.Print("Press Enter for next step, q to stop here: ")
			next, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			if strings.EqualFold(strings.TrimSpace(next), "q") {
				break
			}
		}
		fmt.Println()
	}

	record = normalizeWalkRecord(record)
	if err := saveRecord(*recordPath, record); err != nil {
		return err
	}
	if *learn {
		if err := learnRecord(*dbPath, *recordPath, record); err != nil {
			return err
		}
	}

	fmt.Printf("\nSaved record to %s\n", *recordPath)
	return nil
}

func openDB(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureFTSNodesTable(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func ensureFTSNodesTable(db *sql.DB) error {
	if tableExists(db, "fts_nodes") {
		return nil
	}

	if _, err := db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS fts_nodes USING fts5(
			node_id UNINDEXED,
			doc_id UNINDEXED,
			title,
			summary,
			body,
			code_blocks,
			front_matter,
			tokenize='unicode61 remove_diacritics 2'
		)
	`); err == nil {
		return nil
	}

	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS fts_nodes (
			rowid INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id TEXT NOT NULL,
			doc_id TEXT NOT NULL,
			title TEXT DEFAULT '',
			summary TEXT DEFAULT '',
			body TEXT DEFAULT '',
			code_blocks TEXT DEFAULT '',
			front_matter TEXT DEFAULT ''
		)
	`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_fts_nodes_doc_id ON fts_nodes (doc_id)`)
	return err
}

func tableExists(db *sql.DB, name string) bool {
	var found string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ? LIMIT 1`, name).Scan(&found)
	return err == nil && found == name
}

func isVirtualFTS(db *sql.DB) bool {
	var sqlDef string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'fts_nodes' LIMIT 1`).Scan(&sqlDef)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToUpper(sqlDef), "VIRTUAL TABLE")
}

func loadDocuments(dbPath string) ([]Document, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return loadDocumentsFromDB(db)
}

func loadDocumentsFromDB(db *sql.DB) ([]Document, error) {

	rows, err := db.Query(`
		SELECT doc_id, doc_name, doc_description, source_path, source_type, structure_json
		FROM documents
		ORDER BY doc_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var doc Document
		var structureJSON string
		if err := rows.Scan(&doc.DocID, &doc.DocName, &doc.DocDescription, &doc.SourcePath, &doc.SourceType, &structureJSON); err != nil {
			return nil, err
		}
		if strings.TrimSpace(structureJSON) != "" {
			if err := json.Unmarshal([]byte(structureJSON), &doc.Structure); err != nil {
				return nil, fmt.Errorf("decode structure for %s: %w", doc.DocID, err)
			}
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

func expandSources(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var expanded []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			if _, err := os.Stat(pattern); err == nil {
				matches = []string{pattern}
			}
		}
		for _, match := range matches {
			if seen[match] {
				continue
			}
			seen[match] = true
			expanded = append(expanded, match)
		}
	}
	sort.Strings(expanded)
	return expanded, nil
}

func documentFromFile(path string) (Document, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".markdown":
		return documentFromMarkdown(path)
	case ".json":
		return documentFromJSON(path)
	default:
		return documentFromText(path)
	}
}

func documentFromMarkdown(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	markers := extractMarkdownHeadings(lines)

	docName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if len(markers) == 0 {
		tree := buildTree([]flatMarkdownNode{{
			Title: docName,
			Level: 1,
			Text:  strings.TrimSpace(content),
		}})
		return newDocument(docIDFromPath(path), docName, path, "markdown", tree), nil
	}

	rawNodes := cutMarkdownNodes(markers, lines)
	tree := buildTree(rawNodes)
	return newDocument(docIDFromPath(path), docName, path, "markdown", tree), nil
}

func documentFromJSON(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}

	var record WalkRecord
	if err := json.Unmarshal(data, &record); err == nil && len(record.Steps) > 0 {
		record = normalizeWalkRecord(record)
		doc := documentFromWalkRecord(record, path)
		doc.DocID = docIDFromPath(path)
		doc.DocName = doc.DocID
		return doc, nil
	}

	pretty := string(data)
	var generic any
	if err := json.Unmarshal(data, &generic); err == nil {
		formatted, ferr := json.MarshalIndent(generic, "", "  ")
		if ferr == nil {
			pretty = string(formatted)
		}
	}

	docName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	tree := buildTree([]flatMarkdownNode{{
		Title: docName,
		Level: 1,
		Text:  pretty,
	}})
	return newDocument(docIDFromPath(path), docName, path, "json", tree), nil
}

func documentFromText(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	docName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	tree := buildTree([]flatMarkdownNode{{
		Title: docName,
		Level: 1,
		Text:  string(data),
	}})
	return newDocument(docIDFromPath(path), docName, path, sourceTypeFromExt(filepath.Ext(path)), tree), nil
}

func extractMarkdownHeadings(lines []string) []markdownMarker {
	markers := []markdownMarker{}
	inCode := false
	for idx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if inCode || line == "" {
			continue
		}
		match := markdownHeadingPattern.FindStringSubmatch(line)
		if len(match) == 0 {
			continue
		}
		markers = append(markers, markdownMarker{
			Title: strings.TrimSpace(match[2]),
			Line:  idx,
			Level: len(match[1]),
		})
	}
	return markers
}

func cutMarkdownNodes(markers []markdownMarker, lines []string) []flatMarkdownNode {
	nodes := make([]flatMarkdownNode, 0, len(markers))
	for idx, marker := range markers {
		end := len(lines)
		if idx+1 < len(markers) {
			end = markers[idx+1].Line
		}
		nodes = append(nodes, flatMarkdownNode{
			Title: marker.Title,
			Level: marker.Level,
			Text:  strings.TrimSpace(strings.Join(lines[marker.Line:end], "\n")),
		})
	}
	return nodes
}

func buildTree(nodes []flatMarkdownNode) []Node {
	type stackEntry struct {
		Node  *Node
		Level int
	}

	var roots []*Node
	var stack []stackEntry
	counter := 0

	for _, raw := range nodes {
		node := &Node{
			NodeID: fmt.Sprintf("%d", counter),
			Title:  raw.Title,
			Text:   strings.TrimSpace(raw.Text),
		}
		counter++

		for len(stack) > 0 && stack[len(stack)-1].Level >= raw.Level {
			stack = stack[:len(stack)-1]
		}

		if len(stack) == 0 {
			roots = append(roots, node)
		} else {
			parent := stack[len(stack)-1].Node
			parent.Nodes = append(parent.Nodes, *node)
			node = &parent.Nodes[len(parent.Nodes)-1]
		}
		stack = append(stack, stackEntry{Node: node, Level: raw.Level})
	}

	result := make([]Node, 0, len(roots))
	for _, root := range roots {
		result = append(result, *root)
	}
	return result
}

func newDocument(docID, docName, sourcePath, sourceType string, tree []Node) Document {
	return Document{
		DocID:          docID,
		DocName:        docName,
		DocDescription: generateDocDescription(tree),
		SourcePath:     sourcePath,
		SourceType:     sourceType,
		Structure:      tree,
	}
}

func flattenTree(nodes []Node) []Node {
	out := []Node{}
	var walk func([]Node)
	walk = func(items []Node) {
		for _, item := range items {
			out = append(out, item)
			if len(item.Nodes) > 0 {
				walk(item.Nodes)
			}
		}
	}
	walk(nodes)
	return out
}

func generateDocDescription(structure []Node) string {
	flat := flattenTree(structure)
	titles := []string{}
	firstText := ""
	for _, node := range flat {
		if node.Title != "" && len(titles) < 5 {
			titles = append(titles, node.Title)
		}
		if firstText == "" {
			text := strings.TrimSpace(strings.ReplaceAll(node.Text, "\n", " "))
			if len(text) > 20 {
				firstText = trimForDisplay(text, 160)
			}
		}
	}
	if len(titles) == 0 {
		return firstText
	}
	if firstText == "" {
		return strings.Join(titles, " > ")
	}
	return strings.Join(titles, " > ") + ". " + firstText
}

func saveDocument(db *sql.DB, doc Document) error {
	structureJSON, err := json.Marshal(doc.Structure)
	if err != nil {
		return err
	}
	indexHash := fmt.Sprintf("%x", md5.Sum(structureJSON))
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM nodes WHERE doc_id = ?`, doc.DocID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM fts_nodes WHERE doc_id = ?`, doc.DocID); err != nil {
		return err
	}

	nodeRows := flattenForIndex(doc.Structure, "", 0)
	for _, row := range nodeRows {
		summary := row.Node.Summary
		if summary == "" {
			summary = row.Node.PrefixSummary
		}
		if summary == "" {
			summary = summarizeText(row.Node.Text, 200)
		}
		parsed := parseMDNodeText(row.Node.Text)
		contentHash := fmt.Sprintf("%x", md5.Sum([]byte(row.Node.Text)))

		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO nodes
			 (node_id, doc_id, title, summary, depth, line_start, line_end, parent_node_id, content_hash)
			 VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, ?)`,
			row.Node.NodeID,
			doc.DocID,
			row.Node.Title,
			summary,
			row.Depth,
			nullIfEmpty(row.ParentID),
			contentHash[:16],
		); err != nil {
			return err
		}

		if _, err := tx.Exec(
			`INSERT INTO fts_nodes
			 (node_id, doc_id, title, summary, body, code_blocks, front_matter)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.Node.NodeID,
			doc.DocID,
			row.Node.Title,
			summary,
			parsed.Body,
			parsed.CodeBlocks,
			parsed.FrontMatter,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO documents
		 (doc_id, doc_name, doc_description, source_path, source_type, structure_json, node_count, index_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.DocID,
		doc.DocName,
		doc.DocDescription,
		doc.SourcePath,
		doc.SourceType,
		string(structureJSON),
		len(nodeRows),
		indexHash,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func countNodes(nodes []Node) int {
	total := 0
	for _, node := range nodes {
		total++
		total += countNodes(node.Nodes)
	}
	return total
}

func getAllIndexMeta(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT source_path, file_hash FROM index_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	meta := map[string]string{}
	for rows.Next() {
		var sourcePath string
		var hash string
		if err := rows.Scan(&sourcePath, &hash); err != nil {
			return nil, err
		}
		meta[sourcePath] = hash
	}
	return meta, rows.Err()
}

func setIndexMeta(db *sql.DB, sourcePath, fileHash string) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO index_meta (source_path, file_hash) VALUES (?, ?)`,
		sourcePath,
		fileHash,
	)
	return err
}

func removeMissingDocuments(db *sql.DB, seenSources map[string]bool) error {
	rows, err := db.Query(`SELECT doc_id, source_path FROM documents WHERE source_path != ''`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var docID string
		var sourcePath string
		if err := rows.Scan(&docID, &sourcePath); err != nil {
			return err
		}
		if seenSources[sourcePath] {
			continue
		}
		if _, err := db.Exec(`DELETE FROM nodes WHERE doc_id = ?`, docID); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM fts_nodes WHERE doc_id = ?`, docID); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM documents WHERE doc_id = ?`, docID); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM index_meta WHERE source_path = ?`, sourcePath); err != nil {
			return err
		}
		fmt.Printf("Removed missing source: %s\n", sourcePath)
	}
	return rows.Err()
}

func fileFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

func docIDFromPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return sanitize(name)
}

func sourceTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".md", ".markdown":
		return "markdown"
	case ".json":
		return "json"
	case ".csv":
		return "csv"
	default:
		return "text"
	}
}

type indexedNode struct {
	Node     Node
	ParentID string
	Depth    int
}

type parsedNodeText struct {
	FrontMatter string
	Body        string
	CodeBlocks  string
}

func flattenForIndex(nodes []Node, parentID string, depth int) []indexedNode {
	out := []indexedNode{}
	for _, node := range nodes {
		out = append(out, indexedNode{
			Node:     node,
			ParentID: parentID,
			Depth:    depth,
		})
		if len(node.Nodes) > 0 {
			out = append(out, flattenForIndex(node.Nodes, node.NodeID, depth+1)...)
		}
	}
	return out
}

func summarizeText(text string, threshold int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if len(text) <= threshold {
		return text
	}
	if threshold < 40 {
		return text[:threshold]
	}
	head := text[:min(len(text), threshold)]
	return strings.TrimSpace(head) + "..."
}

func parseMDNodeText(text string) parsedNodeText {
	text = strings.TrimSpace(text)
	if text == "" {
		return parsedNodeText{}
	}

	frontMatter := ""
	remaining := text
	if strings.HasPrefix(remaining, "---\n") {
		if idx := strings.Index(remaining[4:], "\n---\n"); idx >= 0 {
			frontMatter = strings.TrimSpace(remaining[4 : idx+4])
			remaining = remaining[idx+9:]
		}
	}

	codeParts := []string{}
	body := codeFencePattern.ReplaceAllStringFunc(remaining, func(block string) string {
		match := codeFencePattern.FindStringSubmatch(block)
		if len(match) >= 3 {
			codeParts = append(codeParts, strings.TrimSpace(match[2]))
		}
		return ""
	})
	body = strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(body, "\n\n"))

	return parsedNodeText{
		FrontMatter: frontMatter,
		Body:        body,
		CodeBlocks:  strings.Join(codeParts, "\n"),
	}
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func buildFlatNodeMap(doc Document) map[string]FlatNode {
	out := map[string]FlatNode{}
	for _, node := range flattenDocument(doc) {
		out[node.NodeID] = node
	}
	return out
}

func buildFTSMatchExpr(query string) string {
	tokens := splitQuery(query)
	clean := []string{}
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		token = strings.NewReplacer(`"`, "", `'`, "", "(", " ", ")", " ", ":", " ").Replace(token)
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		clean = append(clean, token)
	}
	if len(clean) == 0 {
		return ""
	}
	if len(clean) == 1 {
		return clean[0]
	}
	return strings.Join(clean, " OR ")
}

func searchDocuments(db *sql.DB, docs []Document, query string, top int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || top <= 0 {
		return nil, nil
	}

	docsByID := make(map[string]Document, len(docs))
	nodeMaps := make(map[string]map[string]FlatNode, len(docs))
	for _, doc := range docs {
		docsByID[doc.DocID] = doc
		nodeMaps[doc.DocID] = buildFlatNodeMap(doc)
	}

	if isVirtualFTS(db) {
		matchExpr := buildFTSMatchExpr(query)
		if matchExpr != "" {
			rows, err := db.Query(
				`SELECT f.node_id, f.doc_id, n.title,
				        bm25(fts_nodes, 5.0, 2.0, 10.0, 1.0, 2.0) AS rank_score
				   FROM fts_nodes f
				   JOIN nodes n ON f.node_id = n.node_id AND f.doc_id = n.doc_id
				  WHERE fts_nodes MATCH ?
				  ORDER BY rank_score
				  LIMIT ?`,
				matchExpr,
				top,
			)
			if err == nil {
				defer rows.Close()
				hits := []SearchHit{}
				for rows.Next() {
					var nodeID, docID, title string
					var rankScore float64
					if err := rows.Scan(&nodeID, &docID, &title, &rankScore); err != nil {
						return nil, err
					}
					doc := docsByID[docID]
					node := nodeMaps[docID][nodeID]
					hits = append(hits, SearchHit{
						DocID:   docID,
						DocName: doc.DocName,
						NodeID:  nodeID,
						Title:   title,
						Text:    node.Text,
						Path:    node.Path,
						Score:   int((-rankScore) * 1000),
					})
				}
				if err := rows.Err(); err != nil {
					return nil, err
				}
				if len(hits) > 0 {
					return hits, nil
				}
			}
		}
	}

	tokens := splitQuery(query)
	var hits []SearchHit
	for _, doc := range docs {
		for _, node := range flattenDocument(doc) {
			score := scoreHit(doc, node, tokens)
			if score == 0 {
				continue
			}
			hits = append(hits, SearchHit{
				DocID:   doc.DocID,
				DocName: doc.DocName,
				NodeID:  node.NodeID,
				Title:   node.Title,
				Text:    node.Text,
				Path:    node.Path,
				Score:   score,
			})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].DocName != hits[j].DocName {
			return hits[i].DocName < hits[j].DocName
		}
		return hits[i].NodeID < hits[j].NodeID
	})

	if len(hits) > top {
		hits = hits[:top]
	}
	return hits, nil
}

func pickDocument(db *sql.DB, docs []Document, docID, query string) (Document, string, error) {
	if docID != "" {
		for _, doc := range docs {
			if doc.DocID == docID {
				return doc, "", nil
			}
		}
		return Document{}, "", fmt.Errorf("document not found: %s", docID)
	}

	hits, err := searchDocuments(db, docs, query, 50)
	if err != nil {
		return Document{}, "", err
	}
	if len(hits) == 0 {
		return Document{}, "", fmt.Errorf("no documents matched query %q", query)
	}

	docScores := make(map[string]int)
	for _, hit := range hits {
		docScores[hit.DocID] += hit.Score
	}

	var selected Document
	bestScore := -1
	for _, doc := range docs {
		if score := docScores[doc.DocID]; score > bestScore {
			bestScore = score
			selected = doc
		}
	}
	if bestScore <= 0 {
		return Document{}, "", fmt.Errorf("no documents matched query %q", query)
	}
	return selected, query, nil
}

func extractStepNodes(doc Document) []FlatNode {
	var steps []FlatNode
	var walk func(nodes []Node, path []string)
	walk = func(nodes []Node, path []string) {
		for _, node := range nodes {
			nodePath := append(copySlice(path), node.Title)
			if stepTitlePattern.MatchString(strings.TrimSpace(node.Title)) {
				steps = append(steps, FlatNode{
					DocID:   doc.DocID,
					DocName: doc.DocName,
					NodeID:  node.NodeID,
					Title:   node.Title,
					Text:    strings.TrimSpace(aggregateNodeText(node)),
					Path:    nodePath,
				})
			}
			if len(node.Nodes) > 0 {
				walk(node.Nodes, nodePath)
			}
		}
	}
	walk(doc.Structure, nil)
	return steps
}

func aggregateNodeText(node Node) string {
	parts := []string{}
	if text := strings.TrimSpace(node.Text); text != "" {
		parts = append(parts, text)
	}
	for _, child := range node.Nodes {
		if text := strings.TrimSpace(aggregateNodeText(child)); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func flattenDocument(doc Document) []FlatNode {
	var out []FlatNode
	var walk func(nodes []Node, path []string)
	walk = func(nodes []Node, path []string) {
		for _, node := range nodes {
			nodePath := append(copySlice(path), node.Title)
			out = append(out, FlatNode{
				DocID:   doc.DocID,
				DocName: doc.DocName,
				NodeID:  node.NodeID,
				Title:   node.Title,
				Text:    strings.TrimSpace(node.Text),
				Path:    nodePath,
			})
			if len(node.Nodes) > 0 {
				walk(node.Nodes, nodePath)
			}
		}
	}
	walk(doc.Structure, nil)
	return out
}

func scoreHit(doc Document, node FlatNode, tokens []string) int {
	score := 0
	for _, token := range tokens {
		score += strings.Count(normalize(doc.DocName), token) * 6
		score += strings.Count(normalize(doc.DocDescription), token) * 2
		score += strings.Count(normalize(doc.SourceType), token) * 2
		score += strings.Count(normalize(node.Title), token) * 12
		score += strings.Count(normalize(strings.Join(node.Path, " ")), token) * 2
		score += strings.Count(normalize(node.Text), token)
	}
	return score
}

func saveRecord(path string, record WalkRecord) error {
	record = normalizeWalkRecord(record)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func learnRecord(dbPath, recordPath string, record WalkRecord) error {
	record = normalizeWalkRecord(record)
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	doc := documentFromWalkRecord(record, recordPath)
	if err := saveDocument(db, doc); err != nil {
		return err
	}
	if fp := fileFingerprint(recordPath); fp != "" {
		if err := setIndexMeta(db, recordPath, fp); err != nil {
			return err
		}
	}
	return nil
}

func documentFromWalkRecord(record WalkRecord, sourcePath string) Document {
	record = normalizeWalkRecord(record)
	root := Node{
		NodeID: "0",
		Title:  fmt.Sprintf("排查记录：%s", record.DocName),
		Text: strings.TrimSpace(fmt.Sprintf(
			"生成时间: %s\n查询: %s\n原始文档: %s",
			record.GeneratedAt,
			record.Query,
			record.DocName,
		)),
	}

	nextID := 1
	children := []Node{}
	if strings.TrimSpace(record.Query) != "" {
		children = append(children, Node{
			NodeID: fmt.Sprintf("%d", nextID),
			Title:  "查询",
			Text:   record.Query,
		})
		nextID++
	}
	for _, step := range record.Steps {
		children = append(children, Node{
			NodeID: fmt.Sprintf("%d", nextID),
			Title:  step.Title,
			Text:   buildWalkStepText(step),
		})
		nextID++
	}
	root.Nodes = children

	docID := fmt.Sprintf("record_%s_%s", sanitize(record.DocID), time.Now().Format("20060102_150405"))
	if sourcePath != "" {
		docID = sanitize(strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath)))
	}

	return Document{
		DocID:          docID,
		DocName:        docID,
		DocDescription: fmt.Sprintf("排查记录 %s，步骤数 %d", record.DocName, len(record.Steps)),
		SourcePath:     sourcePath,
		SourceType:     "json",
		Structure:      []Node{root},
	}
}

func buildWalkStepText(step WalkStep) string {
	step = normalizeWalkStep(step)
	var builder strings.Builder
	builder.WriteString(step.Text)
	if step.Note != "" {
		builder.WriteString("\n\n观察记录\n")
		builder.WriteString(step.Note)
	}
	for idx, action := range step.Actions {
		builder.WriteString(fmt.Sprintf("\n\n动作块 %d\n", idx+1))
		builder.WriteString(action.Spec)
		builder.WriteString(fmt.Sprintf("\nexit_code=%d", action.ExitCode))
		if action.TimedOut {
			builder.WriteString("\ntimed_out=true")
		}
		if action.Output != "" {
			builder.WriteString("\noutput:\n")
			builder.WriteString(action.Output)
		}
	}
	return builder.String()
}

func defaultRecordPath(docID string) string {
	stamp := time.Now().Format("20060102_150405")
	return filepath.Join("records", sanitize(docID)+"_"+stamp+".json")
}

func splitQuery(query string) []string {
	query = normalize(query)
	if query == "" {
		return nil
	}
	fields := strings.Fields(query)
	if len(fields) > 0 {
		return fields
	}
	return []string{query}
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func trimForDisplay(s string, limit int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "..."
}

func sanitize(s string) string {
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		" ", "_",
		":", "_",
	)
	return replacer.Replace(strings.TrimSpace(s))
}

func copySlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func extractActionBlocks(text string) []actionBlock {
	matches := codeFencePattern.FindAllStringSubmatch(text, -1)
	blocks := []actionBlock{}
	for _, match := range matches {
		lang := strings.ToLower(strings.TrimSpace(match[1]))
		if !isSupportedActionLang(lang) {
			continue
		}
		body := strings.TrimSpace(match[2])
		if body == "" {
			continue
		}
		blocks = append(blocks, actionBlock{
			Lang: lang,
			Body: body,
		})
	}
	return blocks
}

func executeActionBlock(specText string, timeout time.Duration) (ActionRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	spec, err := parseBuiltinActionSpec(specText)
	if err != nil {
		return ActionRun{
			Engine:   "go_builtin",
			Spec:     strings.TrimSpace(specText),
			ExitCode: 1,
			Output:   "",
			RanAt:    time.Now().UTC().Format(time.RFC3339),
		}, err
	}

	output, err := runBuiltinAction(ctx, spec)
	run := ActionRun{
		Engine:   "go_builtin",
		Spec:     marshalBuiltinActionSpec(spec),
		ExitCode: 0,
		Output:   strings.TrimSpace(output),
		RanAt:    time.Now().UTC().Format(time.RFC3339),
		TimedOut: ctx.Err() == context.DeadlineExceeded,
	}
	if err != nil {
		if run.TimedOut {
			run.ExitCode = 124
			return run, nil
		}
		run.ExitCode = 1
		if strings.TrimSpace(run.Output) == "" {
			run.Output = err.Error()
		} else {
			run.Output = run.Output + "\n" + err.Error()
		}
		return run, nil
	}
	return run, nil
}

func actionRunFailed(run ActionRun) bool {
	return run.TimedOut || run.ExitCode != 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
