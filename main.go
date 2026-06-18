package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	modelsDevURL      = "https://models.dev/api.json"
	defaultCachePath  = "/tmp/opencode-usage-models-dev-api.json"
	defaultCacheTTL   = 12.0
	jsonLineMaxLength = 32 * 1024 * 1024
)

var (
	defaultReportWindows = []reportWindow{
		{title: "Last 1 day", duration: 24 * time.Hour},
		{title: "Last 7 days", duration: 7 * 24 * time.Hour},
		{title: "Last 30 days", duration: 30 * 24 * time.Hour},
		{title: "Last 90 days", duration: 90 * 24 * time.Hour},
	}
	dayDurationPattern = regexp.MustCompile(`([+-]?(?:\d+(?:\.\d*)?|\.\d+))d`)
)

type reportWindow struct {
	title    string
	duration time.Duration
}

type config struct {
	noPricing     bool
	pricingCache  string
	cacheTTLHours float64
	duration      time.Duration
	durationLabel string
}

func (cfg config) reportWindows() []reportWindow {
	if cfg.duration > 0 {
		return []reportWindow{{title: "Last " + cfg.durationLabel, duration: cfg.duration}}
	}

	return defaultReportWindows
}

type jsonObject map[string]any

type usageRecord struct {
	source           string
	provider         string
	model            string
	timestampMillis  int64
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	totalTokens      int64
	storedCost       *float64
	key              string
}

func (r usageRecord) modelKey() string {
	if r.provider != "" && r.model != "" {
		return r.provider + "/" + r.model
	}

	if r.model != "" {
		return r.model
	}

	if r.provider != "" {
		return r.provider
	}

	return "unknown"
}

type totals struct {
	messages         int64
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	totalTokens      int64
	price            float64
}

func (t *totals) add(record usageRecord, price float64) {
	t.messages++
	t.inputTokens += record.inputTokens
	t.outputTokens += record.outputTokens
	t.cacheReadTokens += record.cacheReadTokens
	t.cacheWriteTokens += record.cacheWriteTokens
	t.totalTokens += record.totalTokens
	t.price += price
}

type price struct {
	input      *float64
	output     *float64
	cacheRead  *float64
	cacheWrite *float64
}

type groupKey struct {
	source string
	model  string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, ok := parseFlags(args, stderr)
	if !ok {
		return 2
	}

	skipped := map[string]int{}
	notes := []string{}
	priceNotes := map[string]struct{}{}

	records := collectUsage(skipped)
	prices := buildPricing(records, cfg, &notes)
	now := time.Now()

	fmt.Fprintln(stdout, "Usage Report")
	fmt.Fprintln(stdout, "============")
	fmt.Fprintf(stdout, "Generated: %s\n\n", now.Format(time.RFC3339))

	for _, window := range cfg.reportWindows() {
		cutoffMillis := now.Add(-window.duration).UnixMilli()
		grouped := map[groupKey]totals{}

		for _, record := range records {
			if record.timestampMillis < cutoffMillis {
				continue
			}

			recordPrice := 0.0
			if !cfg.noPricing {
				recordPrice = calculatePrice(record, prices, priceNotes)
			}

			key := groupKey{source: record.source, model: record.modelKey()}
			total := grouped[key]
			total.add(record, recordPrice)
			grouped[key] = total
		}

		printTable(stdout, window.title, grouped)
	}

	printPricingNotes(stdout, cfg, notes, priceNotes, skipped)
	return 0
}

func parseFlags(args []string, stderr io.Writer) (config, bool) {
	cfg := config{pricingCache: defaultCachePath, cacheTTLHours: defaultCacheTTL}

	flags := flag.NewFlagSet("llm-usage", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.BoolVar(&cfg.noPricing, "no-pricing", false, "skip models.dev lookup and only report token counts")
	flags.Func("duration", "only report usage from the last duration (for example 3h, 1h30m, 3d)", func(value string) error {
		duration, err := parseReportDuration(value)
		if err != nil {
			return err
		}

		cfg.duration = duration
		cfg.durationLabel = strings.TrimSpace(value)
		return nil
	})
	flags.StringVar(&cfg.pricingCache, "pricing-cache", defaultCachePath, "cache path for models.dev/api.json")
	flags.Float64Var(&cfg.cacheTTLHours, "cache-ttl-hours", defaultCacheTTL, "hours before refreshing the models.dev cache")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Report Pi, Codex CLI, Claude Code, and OpenCode token usage and cost.")
		fmt.Fprintln(flags.Output())
		fmt.Fprintf(flags.Output(), "Usage: %s [options]\n\n", flags.Name())
		fmt.Fprintln(flags.Output(), "Options:")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return cfg, false
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument: %s\n", flags.Arg(0))
		flags.Usage()
		return cfg, false
	}

	return cfg, true
}

func parseReportDuration(value string) (time.Duration, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, fmt.Errorf("must not be empty")
	}

	duration, err := time.ParseDuration(text)
	if err == nil {
		if duration <= 0 {
			return 0, fmt.Errorf("must be greater than zero")
		}
		return duration, nil
	}

	if !strings.Contains(text, "d") {
		return 0, fmt.Errorf("must be a duration like 3h, 1h30m, or 3d")
	}

	expanded, err := expandDayDurations(text)
	if err != nil {
		return 0, err
	}

	duration, err = time.ParseDuration(expanded)
	if err != nil {
		return 0, fmt.Errorf("must be a duration like 3h, 1h30m, or 3d")
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}

	return duration, nil
}

func expandDayDurations(value string) (string, error) {
	matches := dayDurationPattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value, nil
	}

	var expanded strings.Builder
	last := 0
	for _, match := range matches {
		expanded.WriteString(value[last:match[0]])

		days, err := strconv.ParseFloat(value[match[2]:match[3]], 64)
		if err != nil {
			return "", fmt.Errorf("must be a duration like 3h, 1h30m, or 3d")
		}

		expanded.WriteString(strconv.FormatFloat(days*24, 'f', -1, 64))
		expanded.WriteByte('h')
		last = match[1]
	}
	expanded.WriteString(value[last:])

	return expanded.String(), nil
}

func collectUsage(skipped map[string]int) []usageRecord {
	var records []usageRecord
	records = append(records, parsePi(skipped)...)
	records = append(records, parseCodex(skipped)...)
	records = append(records, parseClaudeCode(skipped)...)
	records = append(records, parseOpenCode(skipped)...)
	return records
}

func parsePi(skipped map[string]int) []usageRecord {
	root := expandPath("~/.pi/agent/sessions")
	if !dirExists(root) {
		return nil
	}

	var records []usageRecord
	for _, path := range walkFiles(root, ".jsonl", skipped, "Pi") {
		for _, item := range readJSONL(path, skipped, "Pi") {
			if stringValue(item["type"]) != "message" {
				continue
			}

			message, ok := asObject(item["message"])
			if !ok || stringValue(message["role"]) != "assistant" {
				continue
			}

			usage, ok := asObject(message["usage"])
			if !ok {
				continue
			}

			inputTokens := number(nested(usage, "input", "inputTokens", "prompt_tokens"))
			outputTokens := number(nested(usage, "output", "outputTokens", "completion_tokens"))
			cacheReadTokens := number(nested(usage, "cacheRead", "cachedInput", "cache.read"))
			cacheWriteTokens := number(nested(usage, "cacheWrite", "cache.write"))
			timestampMillis, ok := parseTimeMillis(firstTruthy(item["timestamp"], message["timestamp"]))
			if !ok {
				skipped["Pi"]++
				continue
			}

			totalTokens := recordTotal(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, nested(usage, "totalTokens", "total_tokens", "total"))
			if totalTokens <= 0 {
				continue
			}

			cost := usage["cost"]
			storedCost := moneyPtr(cost)
			if costObject, ok := asObject(cost); ok {
				storedCost = moneyPtr(costObject["total"])
			}

			records = append(records, usageRecord{
				source:           "Pi",
				provider:         firstString(message["provider"], "pi"),
				model:            firstString(message["model"], message["modelId"], "unknown"),
				timestampMillis:  timestampMillis,
				inputTokens:      inputTokens,
				outputTokens:     outputTokens,
				cacheReadTokens:  cacheReadTokens,
				cacheWriteTokens: cacheWriteTokens,
				totalTokens:      totalTokens,
				storedCost:       storedCost,
				key:              fmt.Sprintf("pi:%s:%s", path, firstString(item["id"], strconv.Itoa(len(records)))),
			})
		}
	}

	return records
}

func parseCodex(skipped map[string]int) []usageRecord {
	roots := []string{expandPath("~/.codex/sessions"), expandPath("~/.codex/archived_sessions")}
	var records []usageRecord

	for _, root := range roots {
		if !dirExists(root) {
			continue
		}

		for _, path := range walkFiles(root, ".jsonl", skipped, "Codex CLI") {
			provider := "openai"
			model := "unknown"

			for _, item := range readJSONL(path, skipped, "Codex CLI") {
				payload, ok := asObject(item["payload"])
				if !ok {
					continue
				}

				switch stringValue(item["type"]) {
				case "session_meta", "turn_context":
					provider = firstString(payload["model_provider"], provider)
					model = firstString(payload["model"], model)
				}

				if stringValue(item["type"]) != "event_msg" || stringValue(payload["type"]) != "token_count" {
					continue
				}

				info, ok := asObject(payload["info"])
				if !ok {
					continue
				}

				usage, ok := asObject(info["last_token_usage"])
				if !ok {
					continue
				}

				inputTokens := number(usage["input_tokens"])
				outputTokens := number(usage["output_tokens"])
				cacheReadTokens := number(usage["cached_input_tokens"])
				cacheWriteTokens := number(firstTruthy(
					usage["cache_creation_input_tokens"],
					usage["cache_write_input_tokens"],
					usage["cache_creation_tokens"],
				))
				timestampMillis, ok := parseTimeMillis(item["timestamp"])
				if !ok {
					skipped["Codex CLI"]++
					continue
				}

				totalTokens := recordTotal(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, usage["total_tokens"])
				if totalTokens <= 0 {
					continue
				}

				records = append(records, usageRecord{
					source:           "Codex CLI",
					provider:         provider,
					model:            model,
					timestampMillis:  timestampMillis,
					inputTokens:      inputTokens,
					outputTokens:     outputTokens,
					cacheReadTokens:  cacheReadTokens,
					cacheWriteTokens: cacheWriteTokens,
					totalTokens:      totalTokens,
					key:              fmt.Sprintf("codex:%s:%s:%d", path, stringValue(item["timestamp"]), len(records)),
				})
			}
		}
	}

	return records
}

func claudeCodeConfigDirs() []string {
	var dirs []string
	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
		dirs = append(dirs, configDir)
	}
	dirs = append(dirs, "~/.claude")
	return uniqueExpandedDirs(dirs)
}

func claudeCodeRecordFromMessage(item jsonObject, path string, index int) (usageRecord, bool) {
	message, ok := asObject(item["message"])
	if !ok {
		message = item
	}

	if stringValue(message["role"]) != "assistant" && stringValue(item["type"]) != "assistant" {
		return usageRecord{}, false
	}

	usage, ok := asObject(message["usage"])
	if !ok {
		return usageRecord{}, false
	}

	inputTokens := number(nested(usage, "input_tokens", "inputTokens", "input"))
	outputTokens := number(nested(usage, "output_tokens", "outputTokens", "output"))
	cacheReadTokens := number(nested(usage, "cache_read_input_tokens", "cache_read_tokens", "cache.read", "cacheRead"))
	cacheWriteTokens := number(nested(usage, "cache_creation_input_tokens", "cache_creation_tokens", "cache.write", "cacheWrite"))
	if cacheWriteTokens == 0 {
		if cacheCreation, ok := asObject(usage["cache_creation"]); ok {
			cacheWriteTokens = number(cacheCreation["ephemeral_5m_input_tokens"]) + number(cacheCreation["ephemeral_1h_input_tokens"])
		}
	}

	timestampMillis, ok := parseTimeMillis(firstTruthy(item["timestamp"], message["timestamp"], message["created_at"], message["created"]))
	if !ok {
		return usageRecord{}, false
	}

	totalTokens := recordTotal(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, nested(usage, "total_tokens", "totalTokens", "total"))
	if totalTokens <= 0 {
		return usageRecord{}, false
	}

	cost := nested(item, "costUSD", "cost_usd", "message.cost.total", "cost")
	storedCost := moneyPtr(cost)
	if costObject, ok := asObject(cost); ok {
		storedCost = moneyPtr(costObject["total"])
	}

	messageID := firstString(item["uuid"], message["id"], fmt.Sprintf("%s:%d", stringValue(item["timestamp"]), index))
	return usageRecord{
		source:           "Claude Code",
		provider:         "anthropic",
		model:            firstString(message["model"], item["model"], "unknown"),
		timestampMillis:  timestampMillis,
		inputTokens:      inputTokens,
		outputTokens:     outputTokens,
		cacheReadTokens:  cacheReadTokens,
		cacheWriteTokens: cacheWriteTokens,
		totalTokens:      totalTokens,
		storedCost:       storedCost,
		key:              fmt.Sprintf("claude-code:%s:%s", path, messageID),
	}, true
}

func parseClaudeCode(skipped map[string]int) []usageRecord {
	records := map[string]usageRecord{}

	for _, directory := range claudeCodeConfigDirs() {
		root := filepath.Join(directory, "projects")
		if !dirExists(root) {
			continue
		}

		for _, path := range walkFiles(root, ".jsonl", skipped, "Claude Code") {
			for index, item := range readJSONL(path, skipped, "Claude Code") {
				record, ok := claudeCodeRecordFromMessage(item, path, index)
				if !ok {
					if stringValue(item["type"]) == "assistant" && item["message"] != nil {
						skipped["Claude Code"]++
					}
					continue
				}
				records[record.key] = record
			}
		}
	}

	return mapValues(records)
}

func openCodeDataDirs() []string {
	var dirs []string
	for _, configPath := range []string{expandPath("~/.config/opencode/opencode.json"), ".opencode.json"} {
		config, ok := readObjectFile(configPath, nil, "")
		if !ok {
			continue
		}

		data, ok := asObject(config["data"])
		if !ok {
			continue
		}

		if directory := stringValue(data["directory"]); directory != "" {
			dirs = append(dirs, directory)
		}
	}

	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "opencode"))
	}
	dirs = append(dirs, "~/.local/share/opencode")

	return uniqueExpandedDirs(dirs)
}

func openCodeRecordFromMessage(data jsonObject, sessionID, messageID string, timeCreated any, sessionModels map[string]string) (usageRecord, bool) {
	tokens, ok := asObject(data["tokens"])
	if stringValue(data["role"]) != "assistant" || !ok {
		return usageRecord{}, false
	}

	cache, _ := asObject(tokens["cache"])
	inputTokens := number(tokens["input"])
	outputTokens := number(tokens["output"]) + number(tokens["reasoning"])
	cacheReadTokens := number(cache["read"])
	cacheWriteTokens := number(cache["write"])
	timestampMillis, ok := parseTimeMillis(firstTruthy(nested(data, "time.created"), timeCreated))
	if !ok {
		return usageRecord{}, false
	}

	totalTokens := recordTotal(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, tokens["total"])
	if totalTokens <= 0 {
		return usageRecord{}, false
	}

	model := firstString(data["modelID"], sessionModels[sessionID], "unknown")
	provider := firstString(data["providerID"], "opencode")
	return usageRecord{
		source:           "OpenCode",
		provider:         provider,
		model:            model,
		timestampMillis:  timestampMillis,
		inputTokens:      inputTokens,
		outputTokens:     outputTokens,
		cacheReadTokens:  cacheReadTokens,
		cacheWriteTokens: cacheWriteTokens,
		totalTokens:      totalTokens,
		storedCost:       moneyPtr(data["cost"]),
		key:              fmt.Sprintf("opencode:%s:%s", sessionID, messageID),
	}, true
}

func parseOpenCodeSQLite(directory string, skipped map[string]int) map[string]usageRecord {
	records := map[string]usageRecord{}
	dbPath := filepath.Join(directory, "opencode.db")
	if !fileExists(dbPath) {
		return records
	}

	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		skipped["OpenCode"]++
		return records
	}
	defer func() {
		if err := db.Close(); err != nil {
			skipped["OpenCode"]++
		}
	}()

	sessionModels := map[string]string{}
	rows, err := db.Query("select id, model from session")
	if err != nil {
		skipped["OpenCode"]++
		return records
	}
	for rows.Next() {
		var sessionID, model any
		if err := rows.Scan(&sessionID, &model); err != nil {
			skipped["OpenCode"]++
			continue
		}

		modelText := stringValue(model)
		if modelText != "" {
			sessionModels[stringValue(sessionID)] = modelText
		}
	}
	if err := closeRows(rows); err != nil {
		skipped["OpenCode"]++
		return records
	}

	rows, err = db.Query("select id, session_id, time_created, data from message")
	if err != nil {
		skipped["OpenCode"]++
		return records
	}
	for rows.Next() {
		var messageID, sessionID, timeCreated, raw any
		if err := rows.Scan(&messageID, &sessionID, &timeCreated, &raw); err != nil {
			skipped["OpenCode"]++
			continue
		}

		data, ok := decodeObject([]byte(stringValue(raw)))
		if !ok {
			skipped["OpenCode"]++
			continue
		}

		record, ok := openCodeRecordFromMessage(data, stringValue(sessionID), stringValue(messageID), timeCreated, sessionModels)
		if ok {
			records[record.key] = record
		}
	}
	if err := closeRows(rows); err != nil {
		skipped["OpenCode"]++
	}

	return records
}

func parseOpenCodeJSON(directory string, skipped map[string]int) map[string]usageRecord {
	records := map[string]usageRecord{}
	sessionModels := map[string]string{}

	sessionRoot := filepath.Join(directory, "storage", "session")
	if dirExists(sessionRoot) {
		for _, path := range walkFiles(sessionRoot, ".json", skipped, "OpenCode") {
			data, ok := readObjectFile(path, skipped, "OpenCode")
			if !ok {
				continue
			}

			sessionID := stringValue(data["id"])
			model := stringValue(data["model"])
			if sessionID != "" && model != "" {
				sessionModels[sessionID] = model
			}
		}
	}

	messageRoot := filepath.Join(directory, "storage", "message")
	if dirExists(messageRoot) {
		for _, path := range walkFiles(messageRoot, ".json", skipped, "OpenCode") {
			data, ok := readObjectFile(path, skipped, "OpenCode")
			if !ok {
				continue
			}

			sessionID := firstString(data["sessionID"], data["session_id"], filepath.Base(filepath.Dir(path)))
			messageID := firstString(data["id"], strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
			record, ok := openCodeRecordFromMessage(data, sessionID, messageID, nil, sessionModels)
			if ok {
				records[record.key] = record
			}
		}
	}

	return records
}

func parseOpenCode(skipped map[string]int) []usageRecord {
	records := map[string]usageRecord{}

	for _, directory := range openCodeDataDirs() {
		if !dirExists(directory) {
			continue
		}

		for key, record := range parseOpenCodeSQLite(directory, skipped) {
			records[key] = record
		}
		for key, record := range parseOpenCodeJSON(directory, skipped) {
			if _, exists := records[key]; !exists {
				records[key] = record
			}
		}
	}

	return mapValues(records)
}

func buildPricing(records []usageRecord, cfg config, notes *[]string) map[string]price {
	if cfg.noPricing {
		*notes = append(*notes, "Pricing lookup skipped with --no-pricing.")
		return nil
	}

	api, cacheNotes, err := loadModelsDev(expandPath(cfg.pricingCache), cfg.cacheTTLHours)
	*notes = append(*notes, cacheNotes...)
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("models.dev lookup failed: %v", err))
		return nil
	}

	root, ok := asObject(api)
	if !ok {
		*notes = append(*notes, "models.dev response was not an object; pricing unavailable.")
		return nil
	}

	providers := map[string]jsonObject{}
	providerIDs := sortedObjectKeys(root)
	for _, providerID := range providerIDs {
		providerData, ok := asObject(root[providerID])
		if ok {
			providers[providerID] = providerData
		}
	}

	prices := map[string]price{}
	for _, needed := range neededModels(records) {
		for _, candidate := range providerCandidates(needed.provider, needed.model, providers, providerIDs) {
			models, ok := asObject(candidate.data["models"])
			if !ok {
				continue
			}

			modelData, _, ok := matchModel(models, candidate.id, needed.model)
			if !ok {
				continue
			}

			modelPrice, ok := extractPrice(modelData)
			if ok {
				prices[needed.fullKey] = modelPrice
				break
			}
		}
	}

	return prices
}

type neededModel struct {
	provider string
	model    string
	fullKey  string
}

func neededModels(records []usageRecord) []neededModel {
	seen := map[string]neededModel{}
	for _, record := range records {
		needed := neededModel{provider: record.provider, model: record.model, fullKey: record.modelKey()}
		seen[needed.provider+"\x00"+needed.model+"\x00"+needed.fullKey] = needed
	}

	models := make([]neededModel, 0, len(seen))
	for _, needed := range seen {
		models = append(models, needed)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].provider != models[j].provider {
			return models[i].provider < models[j].provider
		}
		if models[i].model != models[j].model {
			return models[i].model < models[j].model
		}
		return models[i].fullKey < models[j].fullKey
	})

	return models
}

type providerCandidate struct {
	id   string
	data jsonObject
}

func providerCandidates(provider, model string, providers map[string]jsonObject, providerIDs []string) []providerCandidate {
	seen := map[string]struct{}{}
	candidates := []providerCandidate{}
	add := func(providerID string) {
		if providerID == "" {
			return
		}
		data, ok := providers[providerID]
		if !ok {
			return
		}
		if _, exists := seen[providerID]; exists {
			return
		}
		seen[providerID] = struct{}{}
		candidates = append(candidates, providerCandidate{id: providerID, data: data})
	}

	add(provider)
	add(providerFromModel(model))

	for _, providerID := range providerIDs {
		if provider != "" && (strings.Contains(provider, providerID) || strings.Contains(providerID, provider)) {
			add(providerID)
		}
	}

	return candidates
}

func providerFromModel(model string) string {
	provider, _, ok := strings.Cut(model, "/")
	if !ok || provider == "" {
		return ""
	}
	return provider
}

func matchModel(models jsonObject, providerID, model string) (jsonObject, string, bool) {
	modelCandidates := []string{model}
	if prefix, suffix, ok := strings.Cut(model, "/"); ok && suffix != "" {
		if prefix == providerID || providerID == "" {
			modelCandidates = append(modelCandidates, suffix)
		}
	}

	for _, candidate := range modelCandidates {
		modelData, ok := asObject(models[candidate])
		if ok {
			return modelData, candidate, true
		}
	}

	for _, modelID := range sortedObjectKeys(models) {
		for _, candidate := range modelCandidates {
			if modelID == candidate || strings.HasSuffix(modelID, "/"+candidate) || strings.HasSuffix(candidate, "/"+modelID) {
				modelData, ok := asObject(models[modelID])
				if ok {
					return modelData, modelID, true
				}
			}
		}
	}

	return nil, "", false
}

func extractPrice(modelData jsonObject) (price, bool) {
	cost, ok := asObject(modelData["cost"])
	if !ok {
		cost, ok = asObject(modelData["pricing"])
	}
	if !ok {
		return price{}, false
	}

	modelPrice := price{
		input:      firstMoney(cost, "input", "prompt"),
		output:     firstMoney(cost, "output", "completion"),
		cacheRead:  firstMoney(cost, "cache_read", "cacheRead", "input_cache_read", "cached_input"),
		cacheWrite: firstMoney(cost, "cache_write", "cacheWrite", "input_cache_write", "cache_creation"),
	}

	if modelPrice.input == nil && modelPrice.output == nil && modelPrice.cacheRead == nil && modelPrice.cacheWrite == nil {
		return price{}, false
	}

	return modelPrice, true
}

func calculatePrice(record usageRecord, prices map[string]price, notes map[string]struct{}) float64 {
	modelPrice, ok := prices[record.modelKey()]
	if !ok {
		if record.storedCost != nil {
			notes[fmt.Sprintf("Used stored cost for %s %s; no models.dev match.", record.source, record.modelKey())] = struct{}{}
			return *record.storedCost
		}

		notes[fmt.Sprintf("No pricing match for %s %s; price shown as $0.", record.source, record.modelKey())] = struct{}{}
		return 0
	}

	total := 0.0
	missing := []string{}
	if modelPrice.input != nil {
		total += float64(record.inputTokens) * *modelPrice.input / 1_000_000
	} else if record.inputTokens != 0 {
		missing = append(missing, "input")
	}

	if modelPrice.output != nil {
		total += float64(record.outputTokens) * *modelPrice.output / 1_000_000
	} else if record.outputTokens != 0 {
		missing = append(missing, "output")
	}

	if modelPrice.cacheRead != nil {
		total += float64(record.cacheReadTokens) * *modelPrice.cacheRead / 1_000_000
	} else if modelPrice.input != nil {
		total += float64(record.cacheReadTokens) * *modelPrice.input / 1_000_000
	} else if record.cacheReadTokens != 0 {
		missing = append(missing, "cache read")
	}

	if modelPrice.cacheWrite != nil {
		total += float64(record.cacheWriteTokens) * *modelPrice.cacheWrite / 1_000_000
	} else if modelPrice.input != nil {
		total += float64(record.cacheWriteTokens) * *modelPrice.input / 1_000_000
	} else if record.cacheWriteTokens != 0 {
		missing = append(missing, "cache write")
	}

	if len(missing) > 0 {
		notes[fmt.Sprintf("Missing %s pricing for %s; those components used $0.", strings.Join(missing, ", "), record.modelKey())] = struct{}{}
	}

	return total
}

func loadModelsDev(cachePath string, ttlHours float64) (any, []string, error) {
	var notes []string
	if cachePath != "" {
		cached, note, ok := loadModelsDevCache(cachePath, ttlHours)
		if note != "" {
			notes = append(notes, note)
		}
		if ok {
			return cached, notes, nil
		}
	}

	request, err := http.NewRequest(http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, notes, err
	}
	request.Header.Set("User-Agent", "llm-usage/1.0")

	client := http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, notes, err
	}

	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, notes, readErr
	}
	if closeErr != nil {
		return nil, notes, closeErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, notes, fmt.Errorf("%s returned HTTP %d", modelsDevURL, response.StatusCode)
	}

	if cachePath != "" {
		if err := os.WriteFile(cachePath, raw, 0o644); err != nil {
			notes = append(notes, fmt.Sprintf("Could not write models.dev cache %s: %v", cachePath, err))
		}
	}

	value, err := decodeJSON(raw)
	if err != nil {
		return nil, notes, err
	}

	return value, notes, nil
}

func loadModelsDevCache(cachePath string, ttlHours float64) (any, string, bool) {
	info, err := os.Stat(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", false
		}
		return nil, fmt.Sprintf("Could not stat models.dev cache %s: %v", cachePath, err), false
	}

	if time.Since(info.ModTime()) >= time.Duration(ttlHours*float64(time.Hour)) {
		return nil, "", false
	}

	raw, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, fmt.Sprintf("Could not read models.dev cache %s: %v", cachePath, err), false
	}

	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Sprintf("Could not parse models.dev cache %s: %v", cachePath, err), false
	}

	return value, "", true
}

func printTable(w io.Writer, title string, grouped map[groupKey]totals) {
	headers := []string{"Source", "Model", "Turns", "Input", "Output", "Cached Read", "Cache Write", "Total Tokens", "Price"}
	rows := [][]string{}
	grand := totals{}

	keys := make([]groupKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].source != keys[j].source {
			return keys[i].source < keys[j].source
		}
		return keys[i].model < keys[j].model
	})

	for _, key := range keys {
		total := grouped[key]
		grand.messages += total.messages
		grand.inputTokens += total.inputTokens
		grand.outputTokens += total.outputTokens
		grand.cacheReadTokens += total.cacheReadTokens
		grand.cacheWriteTokens += total.cacheWriteTokens
		grand.totalTokens += total.totalTokens
		grand.price += total.price

		rows = append(rows, []string{
			key.source,
			key.model,
			formatInt(total.messages),
			formatInt(total.inputTokens),
			formatInt(total.outputTokens),
			formatInt(total.cacheReadTokens),
			formatInt(total.cacheWriteTokens),
			formatInt(total.totalTokens),
			formatPrice(total.price),
		})
	}

	rows = append(rows, []string{
		"Total",
		"",
		formatInt(grand.messages),
		formatInt(grand.inputTokens),
		formatInt(grand.outputTokens),
		formatInt(grand.cacheReadTokens),
		formatInt(grand.cacheWriteTokens),
		formatInt(grand.totalTokens),
		formatPrice(grand.price),
	})

	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = len(header)
	}
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], len(value))
		}
	}

	formatRow := func(row []string) string {
		formatted := []string{padRight(row[0], widths[0]), padRight(row[1], widths[1])}
		for i := 2; i < len(row); i++ {
			formatted = append(formatted, padLeft(row[i], widths[i]))
		}
		return strings.Join(formatted, "  ")
	}

	fmt.Fprintln(w, title)
	fmt.Fprintln(w, strings.Repeat("-", len(title)))
	fmt.Fprintln(w, formatRow(headers))

	separator := make([]string, len(widths))
	for i, width := range widths {
		separator[i] = strings.Repeat("-", width)
	}
	fmt.Fprintln(w, strings.Join(separator, "  "))

	for _, row := range rows[:len(rows)-1] {
		fmt.Fprintln(w, formatRow(row))
	}
	fmt.Fprintln(w, strings.Join(separator, "  "))
	fmt.Fprintln(w, formatRow(rows[len(rows)-1]))
	fmt.Fprintln(w)
}

func printPricingNotes(w io.Writer, cfg config, notes []string, priceNotes map[string]struct{}, skipped map[string]int) {
	fmt.Fprintln(w, "Pricing Notes")
	fmt.Fprintln(w, "-------------")
	if !cfg.noPricing {
		fmt.Fprintf(w, "Pricing source: %s\n", modelsDevURL)
		fmt.Fprintln(w, "Pricing units assumed to be USD per 1M tokens.")
	}
	fmt.Fprintln(w, "Codex CLI reasoning output tokens are treated as included in output/total.")
	fmt.Fprintln(w, "Claude Code cache creation tokens are shown as cache write tokens.")
	fmt.Fprintln(w, "OpenCode reasoning tokens are folded into output for display/pricing.")
	fmt.Fprintln(w, "OpenCode data is read from SQLite first, then JSON storage is used only for missing messages.")

	for _, note := range notes {
		fmt.Fprintln(w, note)
	}
	for _, note := range sortedSet(priceNotes) {
		fmt.Fprintln(w, note)
	}
	for _, source := range sortedIntMapKeys(skipped) {
		if skipped[source] > 0 {
			fmt.Fprintf(w, "Skipped malformed/unreadable %s records: %d\n", source, skipped[source])
		}
	}
}

func readJSONL(path string, skipped map[string]int, source string) []jsonObject {
	file, err := os.Open(path)
	if err != nil {
		skipped[source]++
		return nil
	}
	defer func() {
		if err := file.Close(); err != nil {
			skipped[source]++
		}
	}()

	var records []jsonObject
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), jsonLineMaxLength)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		value, err := decodeJSON(line)
		if err != nil {
			skipped[source]++
			continue
		}

		object, ok := asObject(value)
		if ok {
			records = append(records, object)
		}
	}
	if err := scanner.Err(); err != nil {
		skipped[source]++
	}

	return records
}

func readObjectFile(path string, skipped map[string]int, source string) (jsonObject, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if skipped != nil {
			skipped[source]++
		}
		return nil, false
	}

	object, ok := decodeObject(raw)
	if !ok && skipped != nil {
		skipped[source]++
	}
	return object, ok
}

func decodeObject(raw []byte) (jsonObject, bool) {
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, false
	}

	object, ok := asObject(value)
	return object, ok
}

func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}

	return value, nil
}

func asObject(value any) (jsonObject, bool) {
	switch typed := value.(type) {
	case jsonObject:
		return typed, true
	case map[string]any:
		return jsonObject(typed), true
	default:
		return nil, false
	}
}

func nested(data jsonObject, paths ...string) any {
	for _, path := range paths {
		var current any = data
		found := true
		for _, part := range strings.Split(path, ".") {
			object, ok := asObject(current)
			if !ok {
				found = false
				break
			}

			var exists bool
			current, exists = object[part]
			if !exists {
				found = false
				break
			}
		}

		if found && current != nil {
			return current
		}
	}

	return nil
}

func recordTotal(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64, explicitTotal any) int64 {
	total := number(explicitTotal)
	if total > 0 {
		return total
	}

	return inputTokens + outputTokens + cacheReadTokens + cacheWriteTokens
}

func parseTimeMillis(value any) (int64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case json.Number:
		return numericTimeMillis(typed.String())
	case int:
		return numericTimeMillis(strconv.FormatInt(int64(typed), 10))
	case int64:
		return numericTimeMillis(strconv.FormatInt(typed, 10))
	case float64:
		return millisFromFloat(typed), true
	case float32:
		return millisFromFloat(float64(typed)), true
	case []byte:
		return parseTimeMillis(string(typed))
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, false
		}

		if millis, ok := numericTimeMillis(text); ok {
			return millis, true
		}

		for _, layout := range timeLayouts() {
			parsed, err := time.Parse(layout, text)
			if err == nil {
				return parsed.UnixMilli(), true
			}
		}

		for _, layout := range localTimeLayouts() {
			parsed, err := time.ParseInLocation(layout, text, time.Local)
			if err == nil {
				return parsed.UnixMilli(), true
			}
		}
	}

	return 0, false
}

func numericTimeMillis(text string) (int64, bool) {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}

	return millisFromFloat(value), true
}

func millisFromFloat(value float64) int64 {
	if value > 10_000_000_000 {
		return int64(value)
	}

	return int64(value * 1000)
}

func timeLayouts() []string {
	return []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
	}
}

func localTimeLayouts() []string {
	return []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
}

func number(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case bool:
		return 0
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		floating, err := typed.Float64()
		if err != nil {
			return 0
		}
		return int64(floating)
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case []byte:
		return number(string(typed))
	case string:
		floating, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0
		}
		return int64(floating)
	default:
		return 0
	}
}

func money(value any) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case bool:
		return 0, false
	case json.Number:
		floating, err := typed.Float64()
		return floating, err == nil
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case []byte:
		return money(string(typed))
	case string:
		floating, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return floating, err == nil
	default:
		return 0, false
	}
}

func moneyPtr(value any) *float64 {
	parsed, ok := money(value)
	if !ok {
		return nil
	}

	return &parsed
}

func firstMoney(data jsonObject, keys ...string) *float64 {
	for _, key := range keys {
		parsed, ok := money(data[key])
		if ok {
			return &parsed
		}
	}

	return nil
}

func firstTruthy(values ...any) any {
	for _, value := range values {
		if isTruthy(value) {
			return value
		}
	}

	return nil
}

func isTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case []byte:
		return len(typed) > 0
	case json.Number:
		return number(typed) != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	default:
		return true
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		text := stringValue(value)
		if text != "" {
			return text
		}
	}

	return ""
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "true"
		}
		return ""
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func expandPath(path string) string {
	expanded := os.ExpandEnv(path)
	if expanded == "~" || strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if expanded == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(expanded, "~/"))
		}
	}

	return expanded
}

func uniqueExpandedDirs(paths []string) []string {
	seen := map[string]struct{}{}
	var dirs []string

	for _, path := range paths {
		expanded := filepath.Clean(expandPath(path))
		if _, exists := seen[expanded]; exists {
			continue
		}
		seen[expanded] = struct{}{}
		dirs = append(dirs, expanded)
	}

	return dirs
}

func walkFiles(root, extension string, skipped map[string]int, source string) []string {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			skipped[source]++
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != extension {
			return nil
		}

		paths = append(paths, path)
		return nil
	})
	if err != nil {
		skipped[source]++
	}

	return paths
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func closeRows(rows *sql.Rows) error {
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil {
		return rowErr
	}
	return closeErr
}

func sortedObjectKeys(data jsonObject) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedIntMapKeys(data map[string]int) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(data map[string]struct{}) []string {
	values := make([]string, 0, len(data))
	for value := range data {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func mapValues(records map[string]usageRecord) []usageRecord {
	values := make([]usageRecord, 0, len(records))
	for _, record := range records {
		values = append(values, record)
	}
	return values
}

func formatInt(value int64) string {
	negative := value < 0
	if negative {
		value = -value
	}

	digits := strconv.FormatInt(value, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}

	if negative {
		return "-" + digits
	}
	return digits
}

func formatPrice(value float64) string {
	negative := value < 0
	if negative {
		value = -value
	}

	formatted := strconv.FormatFloat(value, 'f', 2, 64)
	integer, fraction, _ := strings.Cut(formatted, ".")
	result := "$" + formatIntString(integer) + "." + fraction
	if negative {
		return "-" + result
	}
	return result
}

func formatIntString(value string) string {
	for i := len(value) - 3; i > 0; i -= 3 {
		value = value[:i] + "," + value[i:]
	}
	return value
}

func padLeft(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return strings.Repeat(" ", width-len(value)) + value
}

func padRight(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}
