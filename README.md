# audit-loop

Automated cross-agent code review. Claude audits your branch, Kiro addresses findings. Loops until approved.

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
- `git`

## Usage

```bash
# Review current branch against main
audit-loop

# Custom base branch
audit-loop --base develop

# More rounds
audit-loop --max-rounds 5

# Preview without running
audit-loop --dry-run
```

## How it works

1. Captures your branch diff (committed + unstaged)
2. Sends diff to Claude for adversarial review
3. If Claude says NEEDS_CHANGES → sends findings to Kiro
4. Kiro fixes what it agrees with, rejects what it doesn't (with reasoning)
5. New diff sent back to Claude (with Kiro's prior response for context)
6. Repeats until APPROVED or max rounds exhausted

No commits are made during the loop. Changes stay unstaged. You commit when satisfied.

## Output

- Live progress in terminal
- Full audit log written to `.audit/reviews/<timestamp>.md`
- Exit 0 = approved, Exit 1 = max rounds hit

## Options

| Flag | Default | Description |
|------|---------|-------------|
| `--max-rounds N` | 3 | Max review iterations |
| `--base BRANCH` | main | Base branch to diff against |
| `--timeout SECS` | 300 | Timeout per agent call |
| `--agent NAME` | mobius | Kiro agent for addressing |
| `--log-dir PATH` | .audit/reviews | Log output directory |
| `--dry-run` | — | Preview without running |

## Environment variables

All flags have env var equivalents: `AUDIT_MAX_ROUNDS`, `AUDIT_BASE`, `AUDIT_TIMEOUT`, `AUDIT_AGENT`, `AUDIT_LOG_DIR`.
