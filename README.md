# audit-loop

Automated cross-agent review loop. Claude critiques, Kiro addresses. Loops until approved.

Works on code diffs (default) or any file content — design docs, specs, proposals, whatever you point it at.

## Install

```bash
go install github.com/mobiusdickus/audit-loop@latest
```

Or build from source:

```bash
git clone https://github.com/mobiusdickus/audit-loop.git
cd audit-loop
go build -o audit-loop .
mv audit-loop ~/.local/bin/
```

## Requirements

- `claude` CLI (Claude Code)
- `kiro-cli` (with agents configured)
- `git` (only in diff mode)

## Usage

```bash
# Code review — diff current branch against main
audit-loop

# Code review — custom base branch
audit-loop --base develop

# Review a specific file instead of a diff
audit-loop --input docs/architecture.md --theme doc-review

# More rounds
audit-loop --max-rounds 5

# Use a custom theme
audit-loop --theme security
audit-loop --theme ./my-theme/

# Preview without running
audit-loop --dry-run
```

## How it works

1. Captures content (git diff by default, or `--input` file)
2. Sends content to Claude with auditor prompt
3. If Claude says NEEDS_CHANGES → sends findings to Kiro
4. Kiro fixes what it agrees with, rejects what it doesn't (with reasoning)
5. Content re-captured and sent back to Claude (with Kiro's prior response)
6. Repeats until APPROVED or max rounds exhausted

No commits are made during the loop. Changes stay unstaged. You decide what to keep.

## Input modes

### Diff mode (default)

Reviews your branch changes. Requires a git repo. Re-captures the diff each round (since Kiro may modify files).

```bash
audit-loop                     # diff against main
audit-loop --base develop      # diff against develop
```

### File mode (`--input`)

Reviews any file's contents. No git required. Re-reads the file each round (in case Kiro edits it).

```bash
audit-loop --input docs/design.md --theme doc-review
audit-loop --input proposal.txt --theme critique
```

## Themes

A theme is a directory with two files:

```
my-theme/
├── auditor.md      # prompt for the critic (Claude)
└── addresser.md    # prompt for the fixer (Kiro)
```

That's it. No config files, no special format.

### Resolution order

For `--theme <value>`:
1. Literal path (if it exists as a directory)
2. `~/.config/audit-loop/themes/<name>/`
3. If no `--theme` specified, uses embedded code-review defaults

### Template variables

| Variable | Available in | Content |
|----------|-------------|---------|
| `{{CONTENT}}` | auditor | The input content (diff or file contents) |
| `{{DIFF}}` | auditor | Alias for `{{CONTENT}}` (backwards compat) |
| `{{PRIOR_RESPONSE}}` | auditor | Addresser's previous round output (empty on round 1) |
| `{{FINDINGS}}` | addresser | Auditor's findings (everything after verdict line) |
| `{{BRANCH}}` | both | Current branch name (empty in file mode) |
| `{{BASE}}` | both | Base branch name |
| `{{ROUND}}` | both | Current round number |

### Example themes

#### Security audit

```bash
mkdir -p ~/.config/audit-loop/themes/security
```

`auditor.md`:
```
You are a security auditor. Review the following code changes for vulnerabilities.

Focus on: injection, auth bypass, secrets exposure, SSRF, path traversal, broken access control.
Ignore: style, performance, missing tests.

{{PRIOR_RESPONSE}}

Output format:
- First line: APPROVED or NEEDS_CHANGES
- If NEEDS_CHANGES, list findings with severity and fix suggestions.

{{CONTENT}}
```

`addresser.md`:
```
You are addressing security audit findings. For each finding, either fix the vulnerability or explain why it's a false positive.

Read the relevant files before making changes.

Findings:
{{FINDINGS}}
```

#### Document review

```bash
mkdir -p ~/.config/audit-loop/themes/doc-review
```

`auditor.md`:
```
You are reviewing a technical document for clarity, accuracy, and completeness.

Check for: logical gaps, unsupported claims, ambiguous language, missing context, contradictions.
Ignore: grammar, formatting.

{{PRIOR_RESPONSE}}

First line: APPROVED or NEEDS_CHANGES
If NEEDS_CHANGES, list issues with specific quotes and suggestions.

Document:
{{CONTENT}}
```

`addresser.md`:
```
You are improving a document based on review feedback. Edit the file directly to address valid concerns. Reject feedback that misunderstands the intent.

Findings:
{{FINDINGS}}
```

#### Architecture critique

`auditor.md`:
```
You are a systems architect reviewing a design for scalability, failure modes, and operational complexity.

Focus on: single points of failure, missing error handling, unclear ownership boundaries, over-engineering.

{{PRIOR_RESPONSE}}

First line: APPROVED or NEEDS_CHANGES

Design:
{{CONTENT}}
```

### The auditor contract

The auditor prompt **must** produce output where the first matching line is either `APPROVED` or `NEEDS_CHANGES`. Everything after that line becomes `{{FINDINGS}}` for the addresser.

## Output

- Live progress in terminal
- Full audit log written to `.audit/reviews/<timestamp>.md`
- Exit 0 = approved, Exit 1 = max rounds hit

## Options

| Flag | Default | Description |
|------|---------|-------------|
| `--max-rounds N` | 3 | Max review iterations |
| `--base BRANCH` | main | Base branch to diff against |
| `--input PATH` | — | File to review (uses file mode instead of diff mode) |
| `--theme NAME` | — | Theme name or path (dir with auditor.md + addresser.md) |
| `--auditor NAME` | claude | Auditor CLI: `claude` or `codex` |
| `--model MODEL` | claude-sonnet-4-6 | Claude model for auditing |
| `--timeout SECS` | 300 | Timeout per agent call |
| `--agent NAME` | mobius | Kiro agent for addressing |
| `--log-dir PATH` | .audit/reviews | Log output directory |
| `--dry-run` | — | Preview without running |

## Environment variables

All flags have env var equivalents: `AUDIT_MAX_ROUNDS`, `AUDIT_BASE`, `AUDIT_INPUT`, `AUDIT_TIMEOUT`, `AUDIT_AGENT`, `AUDIT_THEME`, `AUDIT_AUDITOR`, `AUDIT_MODEL`, `AUDIT_LOG_DIR`.
