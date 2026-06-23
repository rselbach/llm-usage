package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHelpReturnsSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--help) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty help output on stderr", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Usage: llm-usage [options]") {
		t.Fatalf("stderr = %q, want usage text", stderr.String())
	}
}

func TestRunRejectsUnexpectedArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"extra"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run(extra) code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty output", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unexpected argument: extra") {
		t.Fatalf("stderr = %q, want unexpected argument message", stderr.String())
	}
}

func TestParseFlagsDuration(t *testing.T) {
	var stderr bytes.Buffer
	cfg, ok := parseFlags([]string{"--duration", "3d", "--no-pricing"}, &stderr)
	if !ok {
		t.Fatalf("parseFlags returned false: %s", stderr.String())
	}

	windows := cfg.reportWindows()
	if len(windows) != 1 || windows[0].duration != 72*time.Hour {
		t.Fatalf("reportWindows() = %v, want one 72h window", windows)
	}
	if windows[0].title != "Last 3d" {
		t.Fatalf("window title = %q, want Last 3d", windows[0].title)
	}
}

func TestParseFlagsRejectsNonPositiveDuration(t *testing.T) {
	var stderr bytes.Buffer
	_, ok := parseFlags([]string{"--duration", "0s"}, &stderr)
	if ok {
		t.Fatal("parseFlags accepted --duration 0s")
	}
	if !strings.Contains(stderr.String(), "must be greater than zero") {
		t.Fatalf("stderr = %q, want validation message", stderr.String())
	}
}

func TestParseFlagsRejectsMalformedDuration(t *testing.T) {
	for _, value := range []string{"", "soon", "-1d"} {
		t.Run(value, func(t *testing.T) {
			var stderr bytes.Buffer
			_, ok := parseFlags([]string{"--duration", value}, &stderr)
			if ok {
				t.Fatalf("parseFlags accepted --duration %q", value)
			}
			if !strings.Contains(stderr.String(), "duration") && !strings.Contains(stderr.String(), "must") {
				t.Fatalf("stderr = %q, want duration validation message", stderr.String())
			}
		})
	}
}

func TestParseFlagsPricingCacheOptions(t *testing.T) {
	var stderr bytes.Buffer
	cfg, ok := parseFlags([]string{"--pricing-cache", "~/models.json", "--cache-ttl-hours", "24.5"}, &stderr)
	if !ok {
		t.Fatalf("parseFlags returned false: %s", stderr.String())
	}
	if cfg.pricingCache != "~/models.json" {
		t.Fatalf("pricingCache = %q, want raw flag value", cfg.pricingCache)
	}
	if cfg.cacheTTLHours != 24.5 {
		t.Fatalf("cacheTTLHours = %f, want 24.5", cfg.cacheTTLHours)
	}
}

func TestParseReportDuration(t *testing.T) {
	tests := map[string]struct {
		value string
		want  time.Duration
	}{
		"go duration":       {value: "3h", want: 3 * time.Hour},
		"compound duration": {value: "1h30m", want: 90 * time.Minute},
		"days":              {value: "3d", want: 72 * time.Hour},
		"fractional days":   {value: "1.5d", want: 36 * time.Hour},
		"mixed days":        {value: "2d4h30m", want: 52*time.Hour + 30*time.Minute},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseReportDuration(tc.value)
			if err != nil {
				t.Fatalf("parseReportDuration(%q) returned error: %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("parseReportDuration(%q) = %s, want %s", tc.value, got, tc.want)
			}
		})
	}
}

func TestRunNoPricingNoDataPrintsDefaultWindows(t *testing.T) {
	withIsolatedUsageEnvironment(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--no-pricing"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--no-pricing) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Usage Report", "Last 1 day", "Last 7 days", "Last 30 days", "Last 90 days", "Pricing lookup skipped with --no-pricing."} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	duplicateSeparator := "-----  -----  -----  -----  ------  -----------  -----------  ------------  -----\n-----  -----"
	if strings.Contains(output, duplicateSeparator) {
		t.Fatalf("output has back-to-back table separators:\n%s", output)
	}
	if strings.Count(output, "Total") < 4 {
		t.Fatalf("output = %q, want Total row for each default window", output)
	}
}

func TestRunDurationFiltersRecords(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	writeFile(t, filepath.Join(sessionDir, "usage.jsonl"), strings.Join([]string{
		`{"type":"message","timestamp":"` + recent + `","message":{"role":"assistant","provider":"pi","model":"model-new","usage":{"input":10,"output":5}}}`,
		`{"type":"message","timestamp":"` + old + `","message":{"role":"assistant","provider":"pi","model":"model-old","usage":{"input":100,"output":50}}}`,
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := run([]string{"--no-pricing", "--duration", "24h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run duration code = %d, want 0; stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Last 24h") || !strings.Contains(output, "model-new") {
		t.Fatalf("output = %q, want custom window with recent model", output)
	}
	if strings.Contains(output, "model-old") {
		t.Fatalf("output = %q, old model should be filtered out", output)
	}
	if strings.Contains(output, "Last 7 days") {
		t.Fatalf("output = %q, default windows should be replaced by custom duration", output)
	}
}

func TestParseTimeMillis(t *testing.T) {
	tests := map[string]struct {
		value any
		want  int64
	}{
		"unix seconds": {value: jsonNumber("1710000000"), want: 1710000000000},
		"unix millis":  {value: jsonNumber("1710000000123"), want: 1710000000123},
		"rfc3339":      {value: "2024-03-09T16:00:00Z", want: 1710000000000},
		"offset":       {value: "2024-03-09 11:00:00-05:00", want: 1710000000000},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := parseTimeMillis(tc.value)
			if !ok {
				t.Fatalf("parseTimeMillis(%v) returned false", tc.value)
			}
			if got != tc.want {
				t.Fatalf("parseTimeMillis(%v) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseTimeMillisLocalLayout(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.FixedZone("test-zone", -5*60*60)
	t.Cleanup(func() {
		time.Local = originalLocal
	})

	got, ok := parseTimeMillis("2024-03-09 11:00:00")
	if !ok {
		t.Fatal("parseTimeMillis local layout returned false")
	}
	if got != 1710000000000 {
		t.Fatalf("parseTimeMillis local layout = %d, want 1710000000000", got)
	}
}

func TestParsePiRecordsAndSkippedMalformedData(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionDir, "usage.jsonl"), strings.Join([]string{
		`not-json`,
		`{"type":"message","timestamp":"2024-03-09T16:00:00Z","message":{"role":"assistant","provider":"pi","model":"pi-model","usage":{"prompt_tokens":11,"completion_tokens":7,"cache":{"read":3,"write":2},"cost":{"total":0.25}}}}`,
		`{"type":"message","message":{"role":"assistant","model":"missing-time","usage":{"input":1}}}`,
	}, "\n"))

	skipped := map[string]int{}
	records := parsePi(skipped)
	if len(records) != 1 {
		t.Fatalf("parsePi returned %d records, want 1: %#v", len(records), records)
	}
	record := records[0]
	if record.source != "Pi" || record.provider != "pi" || record.model != "pi-model" {
		t.Fatalf("record identity = %#v, want Pi/pi/pi-model", record)
	}
	if record.inputTokens != 11 || record.outputTokens != 7 || record.cacheReadTokens != 3 || record.cacheWriteTokens != 2 || record.totalTokens != 23 {
		t.Fatalf("record tokens = %#v, want aliases and computed total", record)
	}
	if record.storedCost == nil || *record.storedCost != 0.25 {
		t.Fatalf("storedCost = %v, want 0.25", record.storedCost)
	}
	if skipped["Pi"] != 2 {
		t.Fatalf("skipped Pi = %d, want 2", skipped["Pi"])
	}
}

func TestParseCodexRecordsFromActiveAndArchivedSessions(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	active := filepath.Join(home, ".codex", "sessions")
	archived := filepath.Join(home, ".codex", "archived_sessions")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archived, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(active, "active.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"model_provider":"openai","model":"gpt-active"}}`,
		`{"type":"event_msg","timestamp":"2024-03-09T16:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":20,"cached_input_tokens":3,"cache_creation_input_tokens":4}}}}`,
	}, "\n"))
	writeFile(t, filepath.Join(archived, "archived.jsonl"), strings.Join([]string{
		`{"type":"turn_context","payload":{"model_provider":"azure-openai","model":"gpt-archived"}}`,
		`{"type":"event_msg","timestamp":"2024-03-09T16:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1,"output_tokens":2,"total_tokens":99}}}}`,
	}, "\n"))

	records := parseCodex(map[string]int{})
	if len(records) != 2 {
		t.Fatalf("parseCodex returned %d records, want 2: %#v", len(records), records)
	}
	byModel := recordsByModel(records)
	if byModel["gpt-active"].totalTokens != 37 {
		t.Fatalf("active total = %d, want computed 37", byModel["gpt-active"].totalTokens)
	}
	if byModel["gpt-archived"].provider != "azure-openai" || byModel["gpt-archived"].totalTokens != 99 {
		t.Fatalf("archived record = %#v, want provider and explicit total", byModel["gpt-archived"])
	}
}

func TestClaudeCodeRecordFromMessage(t *testing.T) {
	item := jsonObject{
		"type":      "assistant",
		"timestamp": "2024-03-09T16:00:00Z",
		"message": jsonObject{
			"role":  "assistant",
			"model": "claude-3",
			"usage": jsonObject{
				"input_tokens":  8,
				"output_tokens": 9,
				"cache_creation": jsonObject{
					"ephemeral_5m_input_tokens": 2,
					"ephemeral_1h_input_tokens": 3,
				},
			},
		},
		"costUSD": "0.12",
	}

	record, ok := claudeCodeRecordFromMessage(item, "session.jsonl", 4)
	if !ok {
		t.Fatal("claudeCodeRecordFromMessage returned false")
	}
	if record.provider != "anthropic" || record.model != "claude-3" {
		t.Fatalf("record identity = %#v, want anthropic/claude-3", record)
	}
	if record.inputTokens != 8 || record.outputTokens != 9 || record.cacheWriteTokens != 5 || record.totalTokens != 22 {
		t.Fatalf("record tokens = %#v, want cache creation folded into cache write", record)
	}
	if record.storedCost == nil || *record.storedCost != 0.12 {
		t.Fatalf("storedCost = %v, want 0.12", record.storedCost)
	}
}

func TestParseClaudeCodeScansConfiguredAndDefaultProjectDirs(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	configProject := filepath.Join(home, "claude-config", "projects", "one")
	defaultProject := filepath.Join(home, ".claude", "projects", "two")
	if err := os.MkdirAll(configProject, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(defaultProject, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"uuid":"same-id","timestamp":"2024-03-09T16:00:00Z","message":{"role":"assistant","model":"claude-3","usage":{"input_tokens":2,"output_tokens":3}}}`
	writeFile(t, filepath.Join(configProject, "session.jsonl"), record)
	writeFile(t, filepath.Join(defaultProject, "session.jsonl"), `{"uuid":"other-id","timestamp":"2024-03-09T16:00:00Z","message":{"role":"assistant","model":"claude-4","usage":{"input_tokens":5,"output_tokens":7,"total_tokens":20}}}`)

	records := parseClaudeCode(map[string]int{})
	if len(records) != 2 {
		t.Fatalf("parseClaudeCode returned %d records, want 2: %#v", len(records), records)
	}
	byModel := recordsByModel(records)
	if byModel["claude-3"].totalTokens != 5 || byModel["claude-4"].totalTokens != 20 {
		t.Fatalf("records by model = %#v, want configured and default project records", byModel)
	}
}

func TestOpenCodeRecordFromMessage(t *testing.T) {
	data := jsonObject{
		"role":       "assistant",
		"providerID": "anthropic",
		"time":       jsonObject{"created": "2024-03-09T16:00:00Z"},
		"tokens": jsonObject{
			"input":     jsonNumber("100"),
			"output":    jsonNumber("20"),
			"reasoning": jsonNumber("5"),
			"cache": jsonObject{
				"read":  jsonNumber("7"),
				"write": jsonNumber("8"),
			},
		},
		"cost": jsonNumber("0.42"),
	}

	record, ok := openCodeRecordFromMessage(data, "study-room", "paintball", nil, map[string]string{"study-room": "anthropic/claude-sonnet-4"})
	if !ok {
		t.Fatal("openCodeRecordFromMessage returned false")
	}

	if record.provider != "anthropic" {
		t.Fatalf("provider = %q, want anthropic", record.provider)
	}
	if record.outputTokens != 25 {
		t.Fatalf("outputTokens = %d, want 25", record.outputTokens)
	}
	if record.totalTokens != 140 {
		t.Fatalf("totalTokens = %d, want 140", record.totalTokens)
	}
	if record.storedCost == nil || *record.storedCost != 0.42 {
		t.Fatalf("storedCost = %v, want 0.42", record.storedCost)
	}
}

func TestParseOpenCodeJSONUsesSessionModels(t *testing.T) {
	root := t.TempDir()
	sessionRoot := filepath.Join(root, "storage", "session")
	messageRoot := filepath.Join(root, "storage", "message", "study-room")
	if err := os.MkdirAll(sessionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(messageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionRoot, "study-room.json"), `{"id":"study-room","model":"anthropic/claude-sonnet-4"}`)
	writeFile(t, filepath.Join(messageRoot, "paintball.json"), `{"role":"assistant","time":{"created":"2024-03-09T16:00:00Z"},"tokens":{"input":12,"output":4}}`)

	records := parseOpenCodeJSON(root, map[string]int{})
	if len(records) != 1 {
		t.Fatalf("parseOpenCodeJSON returned %d records, want 1: %#v", len(records), records)
	}
	for _, record := range records {
		if record.model != "anthropic/claude-sonnet-4" || record.totalTokens != 16 {
			t.Fatalf("record = %#v, want session model and computed total", record)
		}
	}
}

func TestParseOpenCodeSQLite(t *testing.T) {
	root := t.TempDir()
	createOpenCodeDB(t, filepath.Join(root, "opencode.db"), 30)

	records := parseOpenCodeSQLite(root, map[string]int{})
	if len(records) != 1 {
		t.Fatalf("parseOpenCodeSQLite returned %d records, want 1: %#v", len(records), records)
	}
	for _, record := range records {
		if record.provider != "anthropic" || record.model != "anthropic/claude-sonnet-4" || record.totalTokens != 30 {
			t.Fatalf("record = %#v, want SQLite record with session model and total 30", record)
		}
	}
}

func TestParseOpenCodeSQLiteSkipsMalformedMessageData(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`create table session (id text, model text)`,
		`create table message (id text, session_id text, time_created text, data text)`,
		`insert into session (id, model) values ('study-room', 'model')`,
		`insert into message (id, session_id, time_created, data) values ('bad', 'study-room', '2024-03-09T16:00:00Z', '{')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	skipped := map[string]int{}
	records := parseOpenCodeSQLite(root, skipped)
	if len(records) != 0 {
		t.Fatalf("parseOpenCodeSQLite returned records = %#v, want none", records)
	}
	if skipped["OpenCode"] != 1 {
		t.Fatalf("skipped OpenCode = %d, want 1", skipped["OpenCode"])
	}
}

func TestParseOpenCodePrefersSQLiteOverDuplicateJSON(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	wd := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Fatal(err)
		}
	})

	root := filepath.Join(home, "xdg", "opencode")
	createOpenCodeDB(t, filepath.Join(root, "opencode.db"), 30)
	sessionRoot := filepath.Join(root, "storage", "session")
	messageRoot := filepath.Join(root, "storage", "message", "study-room")
	if err := os.MkdirAll(sessionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(messageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessionRoot, "study-room.json"), `{"id":"study-room","model":"json-model"}`)
	writeFile(t, filepath.Join(messageRoot, "paintball.json"), `{"id":"paintball","sessionID":"study-room","role":"assistant","providerID":"json-provider","modelID":"json-model","time":{"created":"2024-03-09T16:00:00Z"},"tokens":{"input":99,"output":1}}`)

	records := parseOpenCode(map[string]int{})
	if len(records) != 1 {
		t.Fatalf("parseOpenCode returned %d records, want 1 duplicate-resolved record: %#v", len(records), records)
	}
	for _, record := range records {
		if record.totalTokens != 30 || record.model != "anthropic/claude-sonnet-4" {
			t.Fatalf("record = %#v, want SQLite record to win over duplicate JSON", record)
		}
	}
}

func TestOpenCodeDataDirsFromEnvironmentAndConfig(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	wd := t.TempDir()
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Fatal(err)
		}
	})

	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(configDir, "opencode.json"), `{"data":{"directory":"$OPENCODE_CUSTOM"}}`)
	writeFile(t, filepath.Join(wd, ".opencode.json"), `{"data":{"directory":"~/project-opencode"}}`)
	t.Setenv("OPENCODE_CUSTOM", filepath.Join(home, "custom-opencode"))

	dirs := openCodeDataDirs()
	want := []string{
		filepath.Join(home, "custom-opencode"),
		filepath.Join(home, "project-opencode"),
		filepath.Join(home, "xdg", "opencode"),
		filepath.Join(home, ".local", "share", "opencode"),
	}
	if strings.Join(dirs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("openCodeDataDirs() = %#v, want %#v", dirs, want)
	}
}

func TestPrintPricingNotesIncludesAssumptionsAndSortedDynamicNotes(t *testing.T) {
	var output bytes.Buffer
	printPricingNotes(&output, config{}, []string{"cache note"}, map[string]struct{}{
		"z price note": {},
		"a price note": {},
	}, map[string]int{
		"Pi":        2,
		"Codex CLI": 0,
		"OpenCode":  1,
	})

	text := output.String()
	for _, want := range []string{
		"Pricing source: https://models.dev/api.json",
		"Pricing units assumed to be USD per 1M tokens.",
		"Codex CLI reasoning output tokens are treated as included in output/total.",
		"cache note",
		"a price note",
		"z price note",
		"Skipped malformed/unreadable OpenCode records: 1",
		"Skipped malformed/unreadable Pi records: 2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pricing notes missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "a price note") > strings.Index(text, "z price note") {
		t.Fatalf("price notes are not sorted:\n%s", text)
	}
	if strings.Contains(text, "Codex CLI records: 0") {
		t.Fatalf("zero skipped counts should not be printed:\n%s", text)
	}
}

func TestPathAndNumericHelpers(t *testing.T) {
	home := withIsolatedUsageEnvironment(t)
	t.Setenv("LLM_USAGE_TEST_DIR", "from-env")

	if got := expandPath("~/usage"); got != filepath.Join(home, "usage") {
		t.Fatalf("expandPath home = %q, want home path", got)
	}
	if got := expandPath("$LLM_USAGE_TEST_DIR/cache"); got != "from-env/cache" {
		t.Fatalf("expandPath env = %q, want env-expanded path", got)
	}
	dirs := uniqueExpandedDirs([]string{"~/usage", filepath.Join(home, "usage"), "~/other"})
	if len(dirs) != 2 || dirs[0] != filepath.Join(home, "usage") || dirs[1] != filepath.Join(home, "other") {
		t.Fatalf("uniqueExpandedDirs() = %#v, want deduped expanded dirs", dirs)
	}

	if got := number("42.9"); got != 42 {
		t.Fatalf("number string = %d, want truncated 42", got)
	}
	if got := number([]byte("7")); got != 7 {
		t.Fatalf("number bytes = %d, want 7", got)
	}
	if got, ok := money(jsonNumber("1.25")); !ok || got != 1.25 {
		t.Fatalf("money json number = %f, %t; want 1.25 true", got, ok)
	}
	if _, ok := money("not-money"); ok {
		t.Fatal("money accepted non-numeric string")
	}
}

func TestReadObjectFileInvalidJSONIncrementsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	writeFile(t, path, `{`)
	skipped := map[string]int{}

	object, ok := readObjectFile(path, skipped, "OpenCode")
	if ok || object != nil {
		t.Fatalf("readObjectFile returned %#v, %t; want nil false", object, ok)
	}
	if skipped["OpenCode"] != 1 {
		t.Fatalf("skipped OpenCode = %d, want 1", skipped["OpenCode"])
	}
}

func TestPrintTableFormattingAndTotals(t *testing.T) {
	var output bytes.Buffer
	printTable(&output, "Last 1 day", map[groupKey]totals{
		{source: "Pi", model: "b"}:     {messages: 1, inputTokens: 1000, outputTokens: 2, totalTokens: 1002, price: 1.25},
		{source: "Codex", model: "a"}:  {messages: 2, inputTokens: 3000, outputTokens: 4, cacheReadTokens: 5, cacheWriteTokens: 6, totalTokens: 3015, price: 2.5},
		{source: "Claude", model: "c"}: {messages: 1, inputTokens: -1000, outputTokens: 0, totalTokens: -1000, price: -0.5},
	})

	text := output.String()
	for _, want := range []string{"Codex", "Claude", "Pi", "3,000", "-1,000", "$3.25"} {
		if !strings.Contains(text, want) {
			t.Fatalf("table missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "Codex") > strings.Index(text, "Pi") {
		t.Fatalf("table is not sorted by source then model:\n%s", text)
	}
}

func TestCalculatePriceFallsBackToInputForCache(t *testing.T) {
	input := 3.0
	output := 15.0
	record := usageRecord{
		provider:         "openai",
		model:            "gpt-greendale",
		inputTokens:      1_000_000,
		outputTokens:     1_000_000,
		cacheReadTokens:  1_000_000,
		cacheWriteTokens: 1_000_000,
	}
	prices := map[string]price{
		"openai/gpt-greendale": {input: &input, output: &output},
	}

	got := calculatePrice(record, prices, map[string]struct{}{})
	if math.Abs(got-24.0) > 0.000001 {
		t.Fatalf("calculatePrice() = %f, want 24", got)
	}
}

func TestCalculatePriceUsesStoredCostOrNotesMissingMatch(t *testing.T) {
	stored := 0.77
	notes := map[string]struct{}{}
	got := calculatePrice(usageRecord{source: "Pi", provider: "pi", model: "model", storedCost: &stored}, nil, notes)
	if got != stored {
		t.Fatalf("calculatePrice() = %f, want stored cost %f", got, stored)
	}
	if len(notes) != 1 || !strings.Contains(sortedSet(notes)[0], "Used stored cost") {
		t.Fatalf("notes = %#v, want stored cost note", notes)
	}

	notes = map[string]struct{}{}
	got = calculatePrice(usageRecord{source: "Pi", provider: "pi", model: "missing"}, nil, notes)
	if got != 0 {
		t.Fatalf("calculatePrice() = %f, want 0", got)
	}
	if len(notes) != 1 || !strings.Contains(sortedSet(notes)[0], "No pricing match") {
		t.Fatalf("notes = %#v, want no match note", notes)
	}
}

func TestPricingProviderAndModelMatching(t *testing.T) {
	providers := map[string]jsonObject{
		"openai":    {"models": jsonObject{"gpt-4.1": jsonObject{"cost": jsonObject{"input": 2, "output": 8}}}},
		"anthropic": {"models": jsonObject{"claude-sonnet-4": jsonObject{"pricing": jsonObject{"prompt": 3, "completion": 15}}}},
	}
	providerIDs := []string{"anthropic", "openai"}
	candidates := providerCandidates("anthropic-proxy", "anthropic/claude-sonnet-4", providers, providerIDs)
	if len(candidates) != 1 || candidates[0].id != "anthropic" {
		t.Fatalf("providerCandidates() = %#v, want anthropic fallback", candidates)
	}

	modelData, matched, ok := matchModel(providers["anthropic"]["models"].(jsonObject), "anthropic", "anthropic/claude-sonnet-4")
	if !ok || matched != "claude-sonnet-4" {
		t.Fatalf("matchModel() = %#v, %q, %t; want claude-sonnet-4", modelData, matched, ok)
	}
	price, ok := extractPrice(modelData)
	if !ok || price.input == nil || *price.input != 3 || price.output == nil || *price.output != 15 {
		t.Fatalf("extractPrice() = %#v, %t; want prompt/completion pricing", price, ok)
	}
}

func TestBuildPricingUsesFreshCacheWithoutNetwork(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	writeFile(t, cachePath, `{"openai":{"models":{"gpt-4.1":{"cost":{"input":2,"output":8,"cache_read":0.5,"cache_write":1}}}}}`)
	records := []usageRecord{{
		source:           "Codex CLI",
		provider:         "openai",
		model:            "gpt-4.1",
		inputTokens:      1_000_000,
		outputTokens:     2_000_000,
		cacheReadTokens:  3_000_000,
		cacheWriteTokens: 4_000_000,
	}}

	notes := []string{}
	prices := buildPricing(records, config{pricingCache: cachePath, cacheTTLHours: 12}, &notes)
	if len(notes) != 0 {
		t.Fatalf("notes = %#v, want none for fresh valid cache", notes)
	}
	priceNotes := map[string]struct{}{}
	got := calculatePrice(records[0], prices, priceNotes)
	if got != 23.5 {
		t.Fatalf("calculated price = %f, want 23.5", got)
	}
	if len(priceNotes) != 0 {
		t.Fatalf("priceNotes = %#v, want no pricing warnings", priceNotes)
	}
}

func TestBuildPricingSkipsLookupWithoutRecords(t *testing.T) {
	notes := []string{}
	prices := buildPricing(nil, config{pricingCache: filepath.Join(t.TempDir(), "missing.json"), cacheTTLHours: 12}, &notes)
	if prices != nil {
		t.Fatalf("prices = %#v, want nil when there are no records", prices)
	}
	if len(notes) != 1 || notes[0] != "No usage records found; pricing lookup skipped." {
		t.Fatalf("notes = %#v, want no-records skip note", notes)
	}
}

func TestLoadModelsDevCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	writeFile(t, path, `{"openai":{"models":{}}}`)
	value, note, ok := loadModelsDevCache(path, 12)
	if !ok || note != "" {
		t.Fatalf("fresh cache ok=%t note=%q value=%#v, want cache hit", ok, note, value)
	}
	if _, ok := value.(map[string]any); !ok {
		t.Fatalf("value = %#v, want decoded object", value)
	}

	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}
	_, _, ok = loadModelsDevCache(path, 1)
	if ok {
		t.Fatal("stale cache returned ok=true")
	}

	writeFile(t, path, `{`)
	_, note, ok = loadModelsDevCache(path, 12)
	if ok || !strings.Contains(note, "Could not parse") {
		t.Fatalf("invalid cache ok=%t note=%q, want parse note", ok, note)
	}
}

func jsonNumber(value string) any {
	parsed, err := decodeJSON([]byte(value))
	if err != nil {
		panic(err)
	}
	return parsed
}

func withIsolatedUsageEnvironment(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
	return home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func recordsByModel(records []usageRecord) map[string]usageRecord {
	byModel := map[string]usageRecord{}
	for _, record := range records {
		byModel[record.model] = record
	}
	return byModel
}

func createOpenCodeDB(t *testing.T, dbPath string, totalTokens int64) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for _, statement := range []string{
		`create table session (id text, model text)`,
		`create table message (id text, session_id text, time_created text, data text)`,
		`insert into session (id, model) values ('study-room', 'anthropic/claude-sonnet-4')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	data := fmt.Sprintf(`{"role":"assistant","providerID":"anthropic","time":{"created":"2024-03-09T16:00:00Z"},"tokens":{"input":10,"output":5,"cache":{"read":3,"write":2},"total":%d}}`, totalTokens)
	if _, err := db.Exec(`insert into message (id, session_id, time_created, data) values (?, ?, ?, ?)`, "paintball", "study-room", "2024-03-09T16:00:00Z", data); err != nil {
		t.Fatal(err)
	}
}
