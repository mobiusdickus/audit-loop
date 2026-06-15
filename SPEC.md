# audit-loop

Automated cross-agent code review loop. Kiro implements, Claude audits, Kiro addresses. Repeats until approved.

## Usage

```bash
audit-loop                     # review current branch vs main
audit-loop --base develop      # review against different base
audit-loop --max-rounds 5      # override default 3 rounds
audit-loop --dry-run           # show what would be reviewed, don't run
```

## Requirements

- `claude` CLI (Claude Code with `-p` support)
- `kiro-cli` (with `reviewer` and `mobius` agents configured)
- `git`

## Flow

1. Capture diff: `git diff <base>..HEAD` combined with `git diff` (unstaged) — full picture of branch state
2. Send diff to Claude (`claude -p`) with adversarial reviewer prompt + prior round context (if any)
3. Parse response: scan for first line matching `^(APPROVED|NEEDS_CHANGES)` (skip preamble)
4. If APPROVED → write log, exit 0
5. If NEEDS_CHANGES → pass findings to Kiro (`kiro-cli --no-interactive --agent mobius`)
6. Kiro reads findings, fixes code, outputs what it did and why (including rejections with reasoning)
7. Capture new combined diff (`git diff <base>..HEAD` + `git diff` unstaged)
8. Loop back to step 2 — Claude receives the new diff AND Kiro's prior response table (so it can see rejections and decide whether to accept or escalate)
9. If max rounds hit → write log, exit 1

### Rejection handling

When Kiro rejects a finding, the next round's Claude prompt includes Kiro's reasoning. Claude can:
- **Accept** the rejection (stop flagging it)
- **Escalate** with stronger justification (Kiro sees the escalation next round)

If they disagree for the remaining rounds, the loop exits at max rounds and the log captures the unresolved disagreement. This is not a bug — it's a signal for human review.

## Behavior

- No commits during the loop. Changes stay unstaged.
- Live terminal output shows progress per round.
- Full audit log written to `.audit/reviews/<timestamp>.md`
- Each agent invocation wrapped with timeout (default 5 min). On timeout: log it, fail-open.
- Exit 0 = approved. Exit 1 = max rounds exhausted.

## Agents

### Auditor (Claude)
- Invoked via: `claude -p "<prompt>"` (uses user's default Claude Code model)
- Persona: Adversarial code reviewer
- Receives: diff + prior round's Kiro response (if any)
- Output: Strict format — APPROVED/NEEDS_CHANGES + structured findings
- Timeout: 5 min

### Addresser (Kiro)
- Invoked via: `kiro-cli chat --no-interactive --agent mobius --trust-tools=read,write,grep,glob,code,shell "<prompt>"`
- Persona: Mobius (full power — needs shell, write, etc. to fix code)
- Output: What it fixed, what it rejected, reasoning for each
- Timeout: 5 min

## Log Format

One file per run: `.audit/reviews/YYYYMMDD-HHMMSS.md`

```markdown
# Audit Review — <timestamp>

## Meta
- **Branch**: <branch>
- **Base**: <base>
- **Rounds**: <n>/<max>
- **Verdict**: APPROVED | EXHAUSTED
- **Duration**: <time>
- **Diff stats**: <files changed, insertions, deletions>

---

## Round N

### Audit (Claude)
**Verdict**: NEEDS_CHANGES
**Findings**: <count by severity>

#### [severity] — Short description
- **File**: path:line
- **Problem**: description
- **Fix**: suggestion

### Response (Kiro)
| # | Finding | Decision | Reasoning |
|---|---------|----------|-----------|
| 1 | ... | ✅ Fixed / ❌ Rejected | ... |

---

## Result
✅ Approved after N rounds. / ⚠️ Max rounds exhausted. N issues remain.
```

## Configuration

Defaults (overridable via flags or env vars):

| Setting | Default | Flag | Env |
|---------|---------|------|-----|
| Max rounds | 3 | `--max-rounds` | `AUDIT_MAX_ROUNDS` |
| Base branch | main | `--base` | `AUDIT_BASE` |
| Theme | (embedded) | `--theme` | `AUDIT_THEME` |
| Log dir | .audit/reviews | `--log-dir` | `AUDIT_LOG_DIR` |
| Timeout (per agent) | 300s | `--timeout` | `AUDIT_TIMEOUT` |
| Kiro agent | mobius | `--agent` | `AUDIT_AGENT` |

## File Structure

```
audit-loop/
├── audit-loop           # main script (bash)
├── prompts/
│   ├── auditor.md       # claude's review prompt template
│   └── addresser.md     # kiro's fix prompt template
├── lib/
│   ├── parse.sh         # parse APPROVED/NEEDS_CHANGES from output
│   └── log.sh           # markdown log writer
├── README.md
└── .gitignore
```

## Prompt Templates

### auditor.md
```
You are an adversarial code reviewer. Review the following diff.

Focus on: correctness, security, error handling, breaking changes.
Ignore: style, formatting, naming, missing tests (unless critical path).

{{#if prior_response}}
## Prior Round Context

The implementer addressed your previous findings. Here is their response:

{{prior_response}}

If they rejected a finding with valid reasoning, do not re-flag it.
If their reasoning is wrong, escalate with stronger justification.
{{/if}}

Output format (strict):
- First line: APPROVED or NEEDS_CHANGES
- If NEEDS_CHANGES, list findings as:

#### [critical|high|medium|low] — Short description
- **File**: path:line
- **Problem**: what and why
- **Fix**: concrete suggestion

DIFF:
{{diff}}
```

### addresser.md
```
You are addressing code review findings. For each finding below, either fix the code or reject with reasoning.

After making changes, output a summary table:
| # | Finding | Decision | Reasoning |
|---|---------|----------|-----------|

Findings to address:
{{findings}}
```

## Future Considerations

- Support swapping auditor (use kiro `reviewer` agent instead of claude, or gemini via API)
- Support multiple auditors in parallel (claude + gemini, deduplicate findings)
- Integration with CI (run on PR open)
- Configurable severity threshold (only loop on critical/high, accept medium/low)
