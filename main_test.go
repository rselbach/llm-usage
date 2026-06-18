package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

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

func TestParseTimeMillis(t *testing.T) {
	tests := map[string]struct {
		value any
		want  int64
	}{
		"unix seconds": {value: jsonNumber("1710000000"), want: 1710000000000},
		"unix millis":  {value: jsonNumber("1710000000123"), want: 1710000000123},
		"rfc3339":      {value: "2024-03-09T16:00:00Z", want: 1710000000000},
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

func jsonNumber(value string) any {
	parsed, err := decodeJSON([]byte(value))
	if err != nil {
		panic(err)
	}
	return parsed
}
