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
- Invoked via: `kiro-cli chat --no-interactive --trust-tools=read,write,grep,glob,code "<prompt>"`
- Persona: Mobius (needs write, code, etc. to fix code)
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
| Kiro agent | — | `--agent` | `AUDIT_AGENT` |

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

## Discuss Mode

A deliberation mode where both agents independently reason about a question, then debate to converge or surface genuine disagreement.

### Usage

```bash
audit-loop discuss "Should we use a sync.Pool for the diff buffers?"
audit-loop discuss --context main.go "Is the error handling in captureDiff sufficient?"
audit-loop discuss --context "main.go,prompts/" "Should we split this into packages?"
```

### Anti-sycophancy: Blind First Round

LLMs default to agreement when shown well-reasoned prior positions. To force genuine deliberation:

**Round 1** is blind — both agents receive the question + context independently. Neither sees the other's position. This produces two uncontaminated takes.

**Round 2+** each agent sees the other's prior position and must:
1. Steelman the opposing view (articulate the strongest version of it)
2. State where they agree
3. State where they still disagree and why

This makes rubber-stamping structurally difficult.

### Context Parity

Claude runs in print mode (`-p`) — no tool access. Kiro has `read`, `grep`, `glob`, `code`. This asymmetry means Kiro can ground claims in actual code while Claude can only reason from what's in the prompt.

Fix: `--context` files are read and inlined into both agents' prompts. Both see the same code. Kiro *can* look at additional files if it wants to, but Claude always has at minimum what `--context` provides.

### Flow

1. User provides a question + optional `--context` files
2. **Round 1 (blind):**
   - Claude receives question + context → states position
   - Kiro receives question + context → states position independently
   - Neither sees the other
3. **Round 2+:**
   - Claude receives Kiro's prior position → steelmans it, then responds
   - Kiro receives Claude's prior position → steelmans it, then responds
4. Repeat until `CONSENSUS` or max rounds hit

### Exit Conditions

- **CONSENSUS**: Both agents agree in the same round. Log captures the shared conclusion.
- **SPLIT**: Max rounds exhausted, agents did not converge. Log captures both final positions for human decision.

### Output Format (per agent, per round)

```
CONSENSUS | DISAGREE

## Steelman (opposing view)
<strongest case for the other side>

## Position
<own reasoning and conclusion>
```

Round 1 omits the steelman section (nothing to steelman yet).

### CLI Implementation Note

`flag.Parse()` doesn't handle subcommands. Implementation needs:
```go
if len(os.Args) > 1 && os.Args[1] == "discuss" {
    // strip "discuss" from os.Args, parse remaining flags
    // enter discuss loop
}
// otherwise: default audit behavior
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--context PATH` | — | Comma-separated file/dir paths to inline as context |
| `--max-rounds N` | 3 | Max deliberation rounds |
| `--question` | — | Alternative to positional arg for the question |

### Log

Same directory (`.audit/reviews/`), filename prefixed with `discuss-`:

```markdown
# Discussion — <timestamp>

## Question
<user's question>

## Context
<files included>

## Round 1 (blind)

### Claude
<position>

### Kiro
<position>

## Round 2

### Claude
**Verdict**: DISAGREE
**Steelman**: <opposing view>
**Position**: <own view>

### Kiro
**Verdict**: CONSENSUS
**Steelman**: <opposing view>
**Position**: <own view>

## Conclusion
✅ Consensus: <summary> / ⚠️ Split after N rounds. See final positions above.
```

## Future Considerations

- Support swapping auditor (use kiro `reviewer` agent instead of claude, or gemini via API)
- Support multiple auditors in parallel (claude + gemini, deduplicate findings)
- Integration with CI (run on PR open)
- Configurable severity threshold (only loop on critical/high, accept medium/low)
- Discuss mode: support passing stdin or clipboard content as context
