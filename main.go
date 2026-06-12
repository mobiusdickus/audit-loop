package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed prompts/*.md
var prompts embed.FS

var (
	maxRounds = flag.Int("max-rounds", 3, "Max review rounds")
	base      = flag.String("base", "main", "Base branch to diff against")
	model     = flag.String("model", "claude-sonnet-4-6", "Claude model for auditing")
	timeout   = flag.Duration("timeout", 5*time.Minute, "Timeout per agent invocation")
	agent     = flag.String("agent", "mobius", "Kiro agent for addressing findings")
	logDir    = flag.String("log-dir", ".audit/reviews", "Directory for review logs")
	dryRun    = flag.Bool("dry-run", false, "Show what would be reviewed, don't run")
)

func main() {
	flag.Parse()
	loadEnvDefaults()

	paths := flag.Args() // positional args are path filters

	if err := preflight(); err != nil {
		fatal(err.Error())
	}

	branch := gitBranch()
	diff := captureDiff(*base, paths)

	if diff == "" {
		info("No changes to review (branch %s vs %s)", branch, *base)
		os.Exit(0)
	}

	if *dryRun {
		info("Dry run — would review:")
		info("  Branch: %s", branch)
		info("  Base: %s", *base)
		info("  Stats: %s", diffStats(*base, paths))
		info("  Max rounds: %d", *maxRounds)
		os.Exit(0)
	}

	log := newReviewLog(*logDir, branch, *base, *maxRounds)
	start := time.Now()

	var priorResponse string
	var verdict string
	round := 0

	for round < *maxRounds {
		round++
		info("Round %d/%d — sending diff to Claude...", round, *maxRounds)

		diff = captureDiff(*base, paths)
		auditPrompt := buildAuditorPrompt(diff, priorResponse)

		auditOutput, err := runAgent("claude", []string{"-p", auditPrompt, "--model", *model}, *timeout)
		if err != nil {
			errorf("Claude failed (round %d): %v", round, err)
			detail := err.Error()
			if auditOutput != "" {
				detail = auditOutput + "\n\n" + detail
			}
			log.writeRoundAudit(round, "ERROR", detail)
			verdict = "ERROR"
			break
		}

		verdict = parseVerdict(auditOutput)
		findings := parseFindings(auditOutput)

		info("Claude verdict: %s", verdict)
		log.writeRoundAudit(round, verdict, findings)

		if verdict == "APPROVED" {
			break
		}
		if verdict == "UNKNOWN" {
			errorf("Could not parse verdict from Claude's response")
			break
		}

		info("Sending findings to Kiro for resolution...")
		addresserPrompt := buildAddresserPrompt(findings)

		kiroArgs := []string{
			"chat", "--no-interactive",
			"--agent", *agent,
			"--trust-tools=read,write,grep,glob,code",
			addresserPrompt,
		}
		kiroOutput, err := runAgent("kiro-cli", kiroArgs, *timeout)
		if err != nil {
			detail := err.Error()
			if kiroOutput != "" {
				detail = kiroOutput + "\n\n" + detail
			}
			errorf("Kiro failed (round %d): %v", round, detail)
			log.writeRoundResponse("Agent failed: " + detail)
			break
		}

		responseTable := parseResponseTable(kiroOutput)
		priorResponse = responseTable

		info("Kiro addressed findings")
		log.writeRoundResponse(responseTable)
	}

	elapsed := time.Since(start)
	log.finish(round, verdict, elapsed, diffStats(*base, paths))

	info("Done in %s. Log: %s", elapsed.Round(time.Second), log.path)

	if verdict == "APPROVED" {
		os.Exit(0)
	}
	os.Exit(1)
}

// --- Agent runner ---

func runAgent(name string, args []string, t time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	output := stripANSI(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s", t)
	}
	if err != nil {
		return output, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return output, nil
}

// --- Parser ---

var (
	verdictRe  = regexp.MustCompile(`(?m)^(APPROVED|NEEDS_CHANGES)`)
	ansiRe     = regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
	tableRowRe = regexp.MustCompile(`(?m)^\|.+\|$`)
)

func parseVerdict(output string) string {
	m := verdictRe.FindString(output)
	if m == "" {
		return "UNKNOWN"
	}
	return m
}

func parseFindings(output string) string {
	idx := verdictRe.FindStringIndex(output)
	if idx == nil {
		return output
	}
	// Everything after the verdict line
	rest := output[idx[1]:]
	return strings.TrimSpace(rest)
}

func parseResponseTable(output string) string {
	lines := strings.Split(output, "\n")
	var table []string
	inTable := false
	for _, line := range lines {
		if strings.Contains(line, "| #") && strings.Contains(line, "Finding") {
			inTable = true
		}
		if inTable {
			if tableRowRe.MatchString(line) || strings.TrimSpace(line) == "" {
				table = append(table, line)
				if strings.TrimSpace(line) == "" {
					break
				}
			} else if len(table) > 0 {
				break
			}
		}
	}
	return strings.Join(table, "\n")
}

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// --- Prompt builder ---

func buildAuditorPrompt(diff, priorResponse string) string {
	tmpl := mustReadPrompt("prompts/auditor.md")

	var priorCtx string
	if priorResponse != "" {
		priorCtx = fmt.Sprintf(`## Prior Round Context

The implementer addressed your previous findings. Here is their response:

%s

If they rejected a finding with valid reasoning, do not re-flag it.
If their reasoning is wrong, escalate with stronger justification.`, priorResponse)
	}

	// Single-pass replacement against original template to prevent
	// diff content containing placeholders from being substituted.
	r := strings.NewReplacer("{{DIFF}}", diff, "{{PRIOR_RESPONSE}}", priorCtx)
	return r.Replace(tmpl)
}

func buildAddresserPrompt(findings string) string {
	tmpl := mustReadPrompt("prompts/addresser.md")
	return strings.NewReplacer("{{FINDINGS}}", findings).Replace(tmpl)
}

func mustReadPrompt(name string) string {
	data, err := prompts.ReadFile(name)
	if err != nil {
		fatal("missing embedded prompt: %s", name)
	}
	return string(data)
}

// --- Log writer ---

type reviewLog struct {
	path string
	f    *os.File
}

func newReviewLog(dir, branch, base string, maxRounds int) *reviewLog {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cannot create log dir: %v", err)
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, ts+".md")

	f, err := os.Create(path)
	if err != nil {
		fatal("cannot create log: %v", err)
	}

	fmt.Fprintf(f, "# Audit Review — %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "## Meta\n")
	fmt.Fprintf(f, "- **Branch**: %s\n", branch)
	fmt.Fprintf(f, "- **Base**: %s\n", base)
	fmt.Fprintf(f, "- **Max rounds**: %d\n", maxRounds)

	return &reviewLog{path: path, f: f}
}

func (l *reviewLog) writeRoundAudit(round int, verdict, findings string) {
	fmt.Fprintf(l.f, "\n---\n\n## Round %d\n\n", round)
	fmt.Fprintf(l.f, "### Audit (Claude)\n**Verdict**: %s\n\n%s\n", verdict, findings)
}

func (l *reviewLog) writeRoundResponse(response string) {
	fmt.Fprintf(l.f, "\n### Response (Kiro)\n%s\n", response)
}

func (l *reviewLog) finish(rounds int, verdict string, elapsed time.Duration, stats string) {
	fmt.Fprintf(l.f, "\n---\n\n## Meta (final)\n")
	fmt.Fprintf(l.f, "- **Rounds**: %d/%d\n", rounds, *maxRounds)
	fmt.Fprintf(l.f, "- **Verdict**: %s\n", verdict)
	fmt.Fprintf(l.f, "- **Duration**: %s\n", elapsed.Round(time.Second))
	fmt.Fprintf(l.f, "- **Diff stats**: %s\n", stats)
	fmt.Fprintf(l.f, "\n---\n\n## Result\n")

	if verdict == "APPROVED" {
		fmt.Fprintf(l.f, "✅ Approved after %d round(s).\n", rounds)
	} else {
		fmt.Fprintf(l.f, "⚠️ Max rounds exhausted. Unresolved findings remain.\n")
	}
	if err := l.f.Close(); err != nil {
		errorf("log close failed: %v", err)
	}
}

// --- Git helpers ---

func captureDiff(base string, paths []string) string {
	committed := gitCmd(append([]string{"diff", base + "..HEAD", "--"}, paths...)...)
	working := gitCmd(append([]string{"diff", "HEAD", "--"}, paths...)...)
	return committed + working
}

func diffStats(base string, paths []string) string {
	var parts []string
	for _, baseArgs := range [][]string{
		{"diff", "--stat", base + "..HEAD", "--"},
		{"diff", "--stat", "HEAD", "--"},
	} {
		args := append(baseArgs, paths...)
		out := strings.TrimSpace(gitCmd(args...))
		if out != "" {
			lines := strings.Split(out, "\n")
			parts = append(parts, lines[len(lines)-1])
		}
	}
	return strings.Join(parts, " | ")
}

func gitBranch() string {
	return strings.TrimSpace(gitCmd("branch", "--show-current"))
}

func gitCmd(args ...string) string {
	out, _ := exec.Command("git", args...).Output()
	return string(out)
}

// --- Env/config helpers ---

func loadEnvDefaults() {
	if v := os.Getenv("AUDIT_MAX_ROUNDS"); v != "" {
		fmt.Sscanf(v, "%d", maxRounds)
	}
	if v := os.Getenv("AUDIT_BASE"); v != "" {
		*base = v
	}
	if v := os.Getenv("AUDIT_MODEL"); v != "" {
		*model = v
	}
	if v := os.Getenv("AUDIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*timeout = d
		}
	}
	if v := os.Getenv("AUDIT_AGENT"); v != "" {
		*agent = v
	}
	if v := os.Getenv("AUDIT_LOG_DIR"); v != "" {
		*logDir = v
	}
}

func preflight() error {
	for _, cmd := range []string{"claude", "kiro-cli", "git"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("%s not found in PATH", cmd)
		}
	}
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("not inside a git repository")
	}
	if _, err := exec.Command("git", "rev-parse", "--verify", *base).Output(); err != nil {
		return fmt.Errorf("base ref %q not found", *base)
	}
	return nil
}

// --- Output helpers ---

func info(format string, args ...any) {
	fmt.Printf("\033[36m[audit-loop]\033[0m "+format+"\n", args...)
}

func errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\033[31m[audit-loop]\033[0m "+format+"\n", args...)
}

func fatal(format string, args ...any) {
	errorf(format, args...)
	os.Exit(1)
}
