# Classroom Grader — Local Orchestrator

A standalone Go CLI that grades student submissions from Google Classroom using a
local [LM Studio](https://lmstudio.ai) model. No Kiro, no cloud tokens.

It imports the [`google-classroom-mcp`](https://github.com/prokhorind/google-classroom-mcp)
classroom package directly, so no MCP server process needs to be running.

## How it works

```
grade CLI
  ├── fetch submissions   → Google Classroom API (same auth as the MCP server)
  ├── read student files  → submissions/<courseID>/<assignment>/<studentID>/<timestamp>/
  ├── grade each student  → POST to LM Studio /v1/chat/completions
  │                          system prompt: ../prompts/grader.md
  └── write marks.json    → submissions/<courseID>/<assignment>/marks.json
```

## Prerequisites

- Go 1.25+
- LM Studio running with a model loaded and the local server enabled
  (default: `http://localhost:1234/v1`)
- Google OAuth2 credentials + cached token — run `go run ./cmd/auth` in the
  MCP server repo if you haven't already; the token file is reusable here

## Build

```bash
cd orchestrator
go build -o grade ./cmd/grade
```

## Run

```bash
./grade \
  -class       "564576658578" \
  -assignment  "41.SQL" \
  -solution    ../solutions/python/41.sql/sol.sql \
  -workspace   ..
```

Credentials can be passed as flags or environment variables:

```bash
export GOOGLE_CREDENTIALS_FILE=/path/to/.secrets/credentials.json
export GOOGLE_TOKEN_FILE=/path/to/.secrets/token.json

./grade \
  -class      "564576658578" \
  -assignment "41.SQL" \
  -solution   ../solutions/python/41.sql/sol.sql \
  -workspace  ..
```

## Flags

| Flag | Default | Description |
|---|---|---|
| `-class` | _(required)_ | Course name or numeric ID (partial match) |
| `-assignment` | _(required)_ | Assignment name (partial match) |
| `-solution` | _(required)_ | Path to teacher solution file |
| `-workspace` | _(required)_ | Root directory containing `prompts/` and `submissions/` |
| `-credentials` | `$GOOGLE_CREDENTIALS_FILE` | Google OAuth2 credentials.json |
| `-token` | `$GOOGLE_TOKEN_FILE` | Cached OAuth2 token.json |
| `-lm-url` | `http://localhost:1234/v1` | LM Studio API base URL |
| `-lm-model` | _(auto)_ | Model identifier — leave empty to use whatever is loaded |
| `-lm-timeout` | `5m` | Per-student request timeout |
| `-students` | _(all)_ | Comma-separated surnames to grade (e.g. `"Іванов,Петренко"`) |
| `-skip-fetch` | `false` | Skip downloading; grade already-downloaded submissions |

## Output

`marks.json` is written to `submissions/<course_id>/<assignment>/marks.json` —
the same location Kiro writes it, so both workflows stay compatible.

```json
[
  {
    "student_name": "Student",
    "student_id": "1gdgfgfg",
    "mark": 8,
    "deductions": "JOIN condition uses wrong column in task 0; extra WHERE clause in task 2",
    "comment": "Гарна спроба! Більшість запитів правильні, але є кілька помилок у умовах JOIN."
  }
]
```

## Tips

- **Model choice** — instruction-tuned models work best (Llama 3, Mistral, Qwen).
  Use at least 7B for reliable rubric adherence.
- **Re-grading without re-fetching** — add `-skip-fetch` to skip the Google Classroom
  API call and go straight to grading already-downloaded files.
- **Partial grading** — use `-students "Іванов,Петренко"` to grade specific students;
  useful for re-grading after an appeal.
