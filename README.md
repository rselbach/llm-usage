# llm-usage

Simple local usage report for Pi, Codex CLI, Claude Code, and OpenCode.

`llm-usage` scans local session data, totals token usage over the last 1, 7,
30, and 90 days, and estimates cost using pricing from
[`models.dev`](https://models.dev/).

## Installation

Install with Homebrew:

```sh
brew install rselbach/tap/llm-usage
```

## Usage

```sh
llm-usage
```

Options:

```sh
llm-usage --no-pricing
llm-usage --duration 3d
llm-usage --duration 3h
llm-usage --pricing-cache /tmp/models-dev-api.json
llm-usage --cache-ttl-hours 24
```

Build from source:

```sh
go build -o llm-usage .
```

## Data Sources

The tool reads usage data from local files only:

- Pi: `~/.pi/agent/sessions`
- Codex CLI: `~/.codex/sessions` and `~/.codex/archived_sessions`
- Claude Code: `~/.claude/projects` or `CLAUDE_CONFIG_DIR/projects`
- OpenCode: configured OpenCode data directories, XDG data, and
  `~/.local/share/opencode`

Pricing data is fetched from `https://models.dev/api.json` unless
`--no-pricing` is used.

## License

MIT. See `LICENSE`.
