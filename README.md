# Classroom Grader

A local tool that fetches student submissions from Google Classroom and grades them
  using an LLM backend — either a local [LM Studio](https://lmstudio.ai) model or the
[Google Gemini API](https://ai.google.dev/). Two interfaces are available:
a **web UI** (`grade-ui`) and a **CLI** (`grade`).

---

## How it works

```
grade-ui / grade CLI
  ├── authenticate        → Google OAuth2 (credentials.json + token.json)
  ├── fetch submissions   → Google Classroom API
  │     saved to: submissions/<courseID>/<assignment>/<studentID>/<timestamp>/
  ├── grade each student  → LM Studio (local) or Gemini API (cloud)
  │     system prompt:      prompts/grader.md
  │     teacher solution:   uploaded via UI / -solution flag
  └── write results       → submissions/<courseID>/<assignment>/marks.json
```

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Go 1.22+ | `go version` to check |
| **LM Studio** _(or Gemini)_ | LM Studio: running with a model loaded and the local server enabled (default `http://localhost:1234/v1`). Gemini: a valid API key. |
| Google OAuth2 credentials | `credentials.json` downloaded from Google Cloud Console |
| Cached OAuth2 token | `token.json` — generated on first run via the auth flow |

### Google Cloud setup (one-time)

1. Go to [Google Cloud Console](https://console.cloud.google.com/) → **APIs & Services** → **Enabled APIs**
2. Enable **Google Classroom API** and **Google Drive API**
3. Go to **Credentials** → **Create Credentials** → **OAuth 2.0 Client ID** → Desktop app
4. Download the JSON file and save it as `credentials.json`

---

## LLM backends

The tool supports two interchangeable backends, selected with `-llm-backend`.

### LM Studio (default)

Runs fully offline. Requires LM Studio to be running with its local server enabled.

- **Grading model** — any instruction-following model loaded in LM Studio
- **Vision model** — used for PDF-to-text extraction; defaults to `qwen/qwen3-vl-8b`

### Gemini

Calls the Google Gemini API. Requires a valid API key (free tier is sufficient for
most classroom workloads).

Set via `-gemini-api-key` or the `GEMINI_API_KEY` environment variable.
The model is chosen from the live model list returned by the API (e.g. `gemini-2.5-flash`).

---

## Directory layout

By default the UI creates and uses `~/Documents/classroom-grader/` as its workspace.
You can override this with `-workspace`.

The CLI requires `-workspace` explicitly.

```
~/Documents/classroom-grader/     # default workspace (UI) or -workspace path
├── credentials.json               # downloaded from Google Cloud Console
├── token.json                     # generated on first auth, reused automatically
├── prompts/
│   └── grader.md                  # grading system prompt
└── submissions/
    └── <courseID>/
        └── <assignment>/
            ├── <studentID>/
            │   └── <timestamp>/   # files downloaded from Classroom
            └── marks.json         # grading results
```

The binaries are placed in the project root after building:

```
grader-orchestrator/
├── grade        # CLI binary
└── grade-ui     # Web UI binary
```

---

## Build

```bash
go build -o grade ./cmd/grade && go build -o grade-ui ./cmd/ui
```

---

## Web UI (recommended)

### Start

```bash
# Using default workspace (~/ Documents/classroom-grader)
./grade-ui

# Or specify an explicit workspace
./grade-ui -workspace /path/to/workspace

# Using Gemini instead of LM Studio
./grade-ui -llm-backend gemini -gemini-api-key YOUR_KEY
```

Then open **http://localhost:8080** in your browser.

### First-run auth

On first start, the UI will prompt you to complete the Google OAuth2 flow if
`token.json` is not found. Follow the printed URL, paste the auth code, and
`token.json` will be saved automatically. You only need to do this once, or again
if the token expires.

### Fetch & Grade tab

1. Pick a **Course** from the dropdown (loaded live from Google Classroom)
2. Pick an **Assignment**
3. Click **Choose File** next to *Teacher solution file* and select your solution (e.g. `sol.sql`)
4. Optionally filter by student surnames (comma-separated)
5. Click **Fetch & Grade** — submissions are downloaded and graded in real time; logs stream line by line and a progress bar advances per student
6. Results appear in a table at the bottom (color-coded marks, deductions, Ukrainian comment)
7. Click **Copy JSON** to copy the raw `marks.json` content to the clipboard

### Re-grade from Disk tab

Use this when submissions are already downloaded and you want to re-grade without
hitting the Google Classroom API. Useful for:
- Tuning the grader prompt and re-running
- Re-grading a specific student after an appeal
- Testing a different model

1. Pick a **Local assignment** from the dropdown (scanned from `submissions/` on disk)
2. Pick a **version** (timestamp) — the newest is pre-selected
3. Upload the teacher solution file and click **Re-grade**

### Web UI flags

| Flag | Default | Description |
|---|---|---|
| `-workspace` | `~/Documents/classroom-grader` | Root directory for prompts and submissions |
| `-credentials` | `$GOOGLE_CREDENTIALS_FILE` → OS default | Path to `credentials.json` |
| `-token` | `$GOOGLE_TOKEN_FILE` → OS default | Path to `token.json` |
| `-llm-backend` | `lmstudio` | LLM backend: `lmstudio` or `gemini` |
| `-lm-url` | `http://localhost:1234/v1` | LM Studio API base URL |
| `-lm-model` | _(auto)_ | LM Studio grading model identifier |
| `-lm-vision-model` | _(auto)_ | LM Studio vision/OCR model identifier |
| `-gemini-api-key` | `$GEMINI_API_KEY` | Gemini API key |
| `-gemini-model` | _(from UI)_ | Gemini model name (e.g. `gemini-2.5-flash`) |
| `-port` | `8080` | HTTP port |

---

## CLI

The `grade` binary is the original command-line interface. Useful for scripting or
running without a browser.

### Minimal run (LM Studio)

```bash
./grade \
  -class      "11Б(ф/м) Технології" \
  -assignment "41.SQL" \
  -solution   /path/to/solutions/41.sql/sol.sql \
  -workspace  /path/to/workspace \
  -credentials /path/to/credentials.json \
  -token      /path/to/token.json
```

### Minimal run (Gemini)

```bash
./grade \
  -class         "11Б(ф/м) Технології" \
  -assignment    "41.SQL" \
  -solution      /path/to/solutions/41.sql/sol.sql \
  -workspace     /path/to/workspace \
  -llm-backend   gemini \
  -gemini-model  gemini-2.5-flash \
  -gemini-api-key YOUR_KEY
```

### All CLI flags

| Flag | Default | Description |
|---|---|---|
| `-class` | _(required)_ | Course name or numeric ID — supports Cyrillic, partial match |
| `-assignment` | _(required)_ | Assignment name — partial match |
| `-solution` | _(required)_ | Path to teacher solution file |
| `-workspace` | _(required)_ | Root directory containing `prompts/` and `submissions/` |
| `-credentials` | `$GOOGLE_CREDENTIALS_FILE` | Path to `credentials.json` |
| `-token` | `$GOOGLE_TOKEN_FILE` | Path to `token.json` |
| `-mcp-root` | — | Loads credentials + token from `<mcp-root>/.secrets/` |
| `-llm-backend` | `lmstudio` | LLM backend: `lmstudio` or `gemini` |
| `-lm-url` | `http://localhost:1234/v1` | LM Studio API base URL |
| `-lm-model` | _(auto)_ | LM Studio model identifier |
| `-lm-timeout` | `5m` | Per-student LLM request timeout |
| `-gemini-api-key` | `$GEMINI_API_KEY` | Gemini API key |
| `-gemini-model` | _(required with gemini)_ | Gemini model name (e.g. `gemini-2.5-flash`) |
| `-students` | _(all)_ | Comma-separated surnames, e.g. `"Іванов,Петренко"` |
| `-skip-fetch` | `false` | Skip downloading — grade already-downloaded files |

---

## Output

`marks.json` is written to `submissions/<courseID>/<assignment>/marks.json`.

```json
[
  {
    "student_name": "Іваненко Олексій",
    "student_id": "1234567890",
    "mark": 9,
    "deductions": "JOIN condition uses wrong column in task 2",
    "comment": "Гарна спроба! Більшість запитів правильні, але є помилка в умові JOIN."
  }
]
```

Marks are on a **1–12** scale. Sorted alphabetically by student name.
