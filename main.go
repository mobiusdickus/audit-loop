package main

import (
	"bytes"
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
	input     = flag.String("input", "", "File to review (uses file mode instead of diff mode)")
	model     = flag.String("model", "claude-sonnet-4-6", "Claude model for auditing")
	auditor   = flag.String("auditor", "claude", "Auditor CLI: claude or codex")
	timeout   = flag.Duration("timeout", 5*time.Minute, "Timeout per agent invocation")
	agent     = flag.String("agent", "", "Kiro agent for addressing findings")
	logDir    = flag.String("log-dir", ".audit/reviews", "Directory for review logs")
	theme     = flag.String("theme", "", "Theme name or path (directory with auditor.md + addresser.md)")
	dryRun    = flag.Bool("dry-run", false, "Show what would be reviewed, don't run")
	full      = flag.Bool("full", false, "Review entire repo, not just branch diff")
	ctxPaths  = flag.String("context", "", "Comma-separated file/dir paths for discuss context")
)

func main() {
	// Subcommand dispatch before flag.Parse()
	if len(os.Args) > 1 && os.Args[1] == "discuss" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		flag.Parse()
		loadEnvDefaults()
		runDiscuss()
		return
	}

	flag.Parse()
	loadEnvDefaults()

	if *maxRounds <= 0 {
		fatal("max-rounds must be >= 1, got %d", *maxRounds)
	}
	if *timeout <= 0 {
		fatal("timeout must be positive, got %s", *timeout)
	}

	if *auditor != "claude" && *auditor != "codex" {
		fatal("unsupported auditor %q: must be claude or codex", *auditor)
	}

	paths := flag.Args() // positional args are path filters

	fileMode := *input != ""

	if !fileMode {
		if err := preflight(); err != nil {
			fatal(err.Error())
		}
	} else {
		// In file mode, just check the auditor and kiro are available
		for _, cmd := range []string{*auditor, "kiro-cli"} {
			if _, err := exec.LookPath(cmd); err != nil {
				fatal("%s not found in PATH", cmd)
			}
		}
		if _, err := os.Stat(*input); err != nil {
			fatal("input file not found: %v", err)
		}
	}

	branch := ""
	if !fileMode {
		branch = gitBranch()
	}

	content, err := captureContent(fileMode, paths)
	if err != nil {
		fatal(err.Error())
	}
	if content == "" {
		if fileMode {
			info("Input file %s is empty", *input)
		} else {
			info("No changes to review (branch %s vs %s)", branch, *base)
		}
		os.Exit(0)
	}

	if *dryRun {
		info("Dry run — would review:")
		if fileMode {
			info("  Input: %s", *input)
		} else {
			info("  Branch: %s", branch)
			info("  Base: %s", *base)
			info("  Stats: %s", diffStats(*base, paths))
		}
		info("  Max rounds: %d", *maxRounds)
		if *theme != "" {
			info("  Theme: %s", *theme)
		}
		os.Exit(0)
	}

	themeDir := resolveThemeDir()
	if themeDir != "" {
		for _, f := range []string{"auditor.md", "addresser.md"} {
			if _, err := os.Stat(filepath.Join(themeDir, f)); err != nil {
				fatal("theme missing required file %s: %v", f, err)
			}
		}
	}

	log := newReviewLog(*logDir, branch, *base, *maxRounds)
	start := time.Now()

	var priorResponse string
	var verdict string
	round := 0

	for round < *maxRounds {
		round++
		info("Round %d/%d — sending content to %s...", round, *maxRounds, *auditor)

		content, err = captureContent(fileMode, paths)
		if err != nil {
			errorf("content capture failed (round %d): %v", round, err)
			log.writeRoundAudit(round, "ERROR", err.Error())
			verdict = "ERROR"
			break
		}
		if content == "" {
			info("No content remaining — treating as resolved")
			verdict = "APPROVED"
			break
		}
		auditPrompt := buildAuditorPrompt(themeDir, content, priorResponse, branch, round)

		auditorBin, auditorCmdArgs, stdinData := auditorArgs(auditPrompt)
		auditOutput, err := runAgent(auditorBin, auditorCmdArgs, stdinData, *timeout)
		if err != nil {
			errorf("%s failed (round %d): %v", *auditor, round, err)
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

		info("%s verdict: %s", *auditor, verdict)
		log.writeRoundAudit(round, verdict, findings)

		if verdict == "APPROVED" {
			break
		}
		if verdict == "UNKNOWN" {
			errorf("Could not parse verdict from Claude's response")
			break
		}

		info("Sending findings to Kiro for resolution...")
		addresserPrompt := buildAddresserPrompt(themeDir, findings, branch, round)

		kiroArgs := []string{
			"chat", "--no-interactive",
			"--trust-tools=read,write,grep,glob,code",
		}
		if *agent != "" {
			kiroArgs = append(kiroArgs, "--agent", *agent)
		} else {
			info("No --agent specified; using kiro-cli default agent")
		}
		kiroOutput, err := runAgent("kiro-cli", kiroArgs, addresserPrompt, *timeout)
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
	stats := ""
	if !fileMode {
		stats = diffStats(*base, paths)
	} else {
		stats = *input
	}
	log.finish(round, verdict, elapsed, stats)

	info("Done in %s. Log: %s", elapsed.Round(time.Second), log.path)

	if verdict == "APPROVED" {
		os.Exit(0)
	}
	os.Exit(1)
}

// --- Discuss mode ---

var discussVerdictRe = regexp.MustCompile(`(?m)^(CONSENSUS|DISAGREE)`)

func runDiscuss() {
	question := strings.Join(flag.Args(), " ")
	if question == "" {
		fatal("usage: audit-loop discuss [--context files] \"question\"")
	}

	if *maxRounds < 1 {
		fatal("--max-rounds must be at least 1")
	}

	if err := preflightTools(); err != nil {
		fatal(err.Error())
	}

	fileContext := loadContext(*ctxPaths)

	if *dryRun {
		info("Dry run — would discuss:")
		info("  Question: %s", question)
		info("  Context: %s", *ctxPaths)
		info("  Max rounds: %d", *maxRounds)
		os.Exit(0)
	}

	dlog := newDiscussLog(*logDir, question, *ctxPaths, *maxRounds)
	start := time.Now()

	var claudePosition, kiroPosition string
	var verdict string

	for round := 1; round <= *maxRounds; round++ {
		if round == 1 {
			// Blind first round — both agents respond independently
			info("Round 1/%d — blind positions...", *maxRounds)

			claudePrompt := buildDiscussPrompt("discuss-claude.md", question, fileContext, "", true)
			kiroPrompt := buildDiscussPrompt("discuss-kiro.md", question, fileContext, "", true)

			var claudeErr, kiroErr error
			claudePosition, claudeErr = runAgent("claude", []string{"-p", "--model", *model}, claudePrompt, *timeout)
			if claudeErr != nil {
				errorf("Claude failed: %v", claudeErr)
				verdict = "ERROR"
				break
			}

			kiroArgs := []string{"chat", "--no-interactive", "--trust-tools=read,grep,glob,code"}
			if *agent != "" {
				kiroArgs = append(kiroArgs, "--agent", *agent)
			}
			kiroPosition, kiroErr = runAgent("kiro-cli", kiroArgs, kiroPrompt, *timeout)
			if kiroErr != nil {
				errorf("Kiro failed: %v", kiroErr)
				verdict = "ERROR"
				break
			}

			dlog.writeRound(round, claudePosition, kiroPosition)
			info("  Claude: %s", parseDiscussVerdict(claudePosition))
			info("  Kiro: %s", parseDiscussVerdict(kiroPosition))
		} else {
			// Debate rounds — each sees the other's prior position
			info("Round %d/%d — debate...", round, *maxRounds)

			claudePrompt := buildDiscussPrompt("discuss-claude.md", question, fileContext, kiroPosition, false)
			prevClaudePosition := claudePosition
			var claudeErr error
			claudePosition, claudeErr = runAgent("claude", []string{"-p", "--model", *model}, claudePrompt, *timeout)
			if claudeErr != nil {
				errorf("Claude failed (round %d): %v", round, claudeErr)
				verdict = "ERROR"
				break
			}

			kiroPrompt := buildDiscussPrompt("discuss-kiro.md", question, fileContext, prevClaudePosition, false)
			kiroArgs := []string{"chat", "--no-interactive", "--trust-tools=read,grep,glob,code"}
			if *agent != "" {
				kiroArgs = append(kiroArgs, "--agent", *agent)
			}
			var kiroErr error
			kiroPosition, kiroErr = runAgent("kiro-cli", kiroArgs, kiroPrompt, *timeout)
			if kiroErr != nil {
				errorf("Kiro failed (round %d): %v", round, kiroErr)
				verdict = "ERROR"
				break
			}

			dlog.writeRound(round, claudePosition, kiroPosition)
			info("  Claude: %s", parseDiscussVerdict(claudePosition))
			info("  Kiro: %s", parseDiscussVerdict(kiroPosition))
		}

		// Check if both reached consensus
		cv := parseDiscussVerdict(claudePosition)
		kv := parseDiscussVerdict(kiroPosition)
		if cv == "CONSENSUS" && kv == "CONSENSUS" {
			verdict = "CONSENSUS"
			break
		}
		// If either says CONSENSUS in a later round, keep going to confirm the other agrees
		if round == *maxRounds {
			verdict = "SPLIT"
		}
	}

	elapsed := time.Since(start)
	dlog.finish(verdict, elapsed)
	info("Done in %s. Outcome: %s. Log: %s", elapsed.Round(time.Second), verdict, dlog.path)

	if verdict == "CONSENSUS" {
		os.Exit(0)
	}
	os.Exit(1)
}

func buildDiscussPrompt(tmplName, question, fileContext, priorPosition string, blind bool) string {
	tmpl := loadPrompt("", tmplName)

	var ctxBlock string
	if fileContext != "" {
		var indented strings.Builder
		indented.WriteString("## Code Context\n")
		for _, line := range strings.Split(fileContext, "\n") {
			indented.WriteString("    " + line + "\n")
		}
		ctxBlock = indented.String()
	}

	var priorBlock, format string
	if blind {
		format = "## Position\n<your reasoning and conclusion>"
	} else {
		priorBlock = fmt.Sprintf("## Other Agent's Position\n%s\n", priorPosition)
		format = "## Steelman (opposing view)\n<strongest case for their position>\n\n## Position\n<your reasoning and conclusion>"
	}

	return strings.NewReplacer(
		"{{QUESTION}}", question,
		"{{CONTEXT}}", ctxBlock,
		"{{PRIOR_ROUND}}", priorBlock,
		"{{FORMAT}}", format,
	).Replace(tmpl)
}

const (
	maxFileSize    = 100 << 10 // 100 KB per file
	maxContextSize = 512 << 10 // 512 KB total
)

func loadContext(paths string) string {
	if paths == "" {
		return ""
	}
	var parts []string
	var total int

	addFile := func(path string) error {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			errorf("cannot resolve %q: %v", path, err)
			return nil
		}
		if isSensitivePath(real) {
			info("skipping sensitive file: %s", path)
			return nil
		}
		fi, err := os.Stat(real)
		if err != nil {
			errorf("cannot read context %q: %v", path, err)
			return nil
		}
		if fi.Size() > maxFileSize {
			errorf("skipping %q: exceeds %d KB limit", path, maxFileSize>>10)
			return nil
		}
		data, err := os.ReadFile(real)
		if err != nil {
			errorf("cannot read context %q: %v", path, err)
			return nil
		}
		if bytes.ContainsRune(data, 0) {
			errorf("skipping %q: binary file", path)
			return nil
		}
		if total+len(data) > maxContextSize {
			return fmt.Errorf("total context exceeds %d KB limit", maxContextSize>>10)
		}
		total += len(data)
		parts = append(parts, fmt.Sprintf("// %s\n%s", path, string(data)))
		return nil
	}

	for _, p := range strings.Split(paths, ",") {
		p = strings.TrimSpace(p)
		fi, err := os.Stat(p)
		if err != nil {
			errorf("cannot read context %q: %v", p, err)
			continue
		}
		if fi.IsDir() {
			werr := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == ".git" || d.Name() == "node_modules" {
						return filepath.SkipDir
					}
					return nil
				}
				if isSensitivePath(path) {
					info("skipping sensitive file: %s", path)
					return nil
				}
				if e := addFile(path); e != nil {
					return e
				}
				return nil
			})
			if werr != nil {
				errorf("walk %q: %v", p, werr)
				break
			}
			continue
		}
		if e := addFile(p); e != nil {
			errorf("%v", e)
			break
		}
	}
	return strings.Join(parts, "\n\n")
}

func isSensitivePath(path string) bool {
	name := filepath.Base(path)
	lower := strings.ToLower(name)
	switch lower {
	case ".env", ".env.local", ".env.production", ".env.development",
		"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
		"credentials.json", ".npmrc", ".pypirc",
		"credentials", "kubeconfig", "config.json":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".jks", ".tfvars", ".token"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	if strings.HasPrefix(lower, ".env.") {
		return true
	}
	// Catch files inside .ssh/ directories
	if strings.Contains(filepath.ToSlash(path), ".ssh/") {
		return true
	}
	return false
}

func parseDiscussVerdict(output string) string {
	m := discussVerdictRe.FindStringSubmatch(output)
	if m == nil {
		return "DISAGREE"
	}
	return m[1]
}

// --- Discuss log writer ---

type discussLog struct {
	path string
	f    *os.File
}

func newDiscussLog(dir, question, ctxPaths string, maxRounds int) *discussLog {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("cannot create log dir: %v", err)
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, "discuss-"+ts+".md")

	f, err := os.Create(path)
	if err != nil {
		fatal("cannot create log: %v", err)
	}

	fmt.Fprintf(f, "# Discussion — %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(f, "## Question\n%s\n\n", question)
	if ctxPaths != "" {
		fmt.Fprintf(f, "## Context\n%s\n\n", ctxPaths)
	}
	fmt.Fprintf(f, "## Meta\n- **Max rounds**: %d\n", maxRounds)

	return &discussLog{path: path, f: f}
}

func (l *discussLog) writeRound(round int, claudeOutput, kiroOutput string) {
	label := fmt.Sprintf("Round %d", round)
	if round == 1 {
		label = "Round 1 (blind)"
	}
	fmt.Fprintf(l.f, "\n---\n\n## %s\n\n", label)
	fmt.Fprintf(l.f, "### Claude\n%s\n\n", strings.TrimSpace(claudeOutput))
	fmt.Fprintf(l.f, "### Kiro\n%s\n", strings.TrimSpace(kiroOutput))
}

func (l *discussLog) finish(verdict string, elapsed time.Duration) {
	fmt.Fprintf(l.f, "\n---\n\n## Conclusion\n")
	fmt.Fprintf(l.f, "- **Outcome**: %s\n", verdict)
	fmt.Fprintf(l.f, "- **Duration**: %s\n", elapsed.Round(time.Second))
	switch verdict {
	case "CONSENSUS":
		fmt.Fprintf(l.f, "\n✅ Consensus reached.\n")
	case "ERROR":
		fmt.Fprintf(l.f, "\n⚠️ Agent error — review halted early.\n")
	default:
		fmt.Fprintf(l.f, "\n⚠️ No consensus. See final positions above.\n")
	}
	if err := l.f.Close(); err != nil {
		errorf("log close failed: %v", err)
	}
}

func auditorArgs(prompt string) (string, []string, string) {
	switch *auditor {
	case "codex":
		return "codex", []string{"exec", "-"}, prompt
	default: // "claude"
		return "claude", []string{"-p", "--model", *model}, prompt
	}
}

func runAgent(name string, args []string, stdinData string, t time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	if stdinData != "" {
		cmd.Stdin = strings.NewReader(stdinData)
	}
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
	if len(table) == 0 {
		errorf("could not extract response table; passing full Kiro output as prior context")
		return output
	}
	return strings.Join(table, "\n")
}

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// --- Theme / Prompt builder ---

func resolveThemeDir() string {
	if *theme == "" {
		return "" // use embedded defaults
	}
	// If path exists as-is, use it
	if info, err := os.Stat(*theme); err == nil && info.IsDir() {
		return *theme
	}
	// Check ~/.config/audit-loop/themes/<name>/
	home, err := os.UserHomeDir()
	if err == nil {
		themesRoot := filepath.Join(home, ".config", "audit-loop", "themes")
		dir, _ := filepath.Abs(filepath.Join(themesRoot, *theme))
		if !strings.HasPrefix(dir+string(os.PathSeparator), themesRoot+string(os.PathSeparator)) {
			fatal("theme path %q escapes themes directory", *theme)
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			// Resolve symlinks and re-check containment
			realDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				fatal("cannot resolve theme path: %v", err)
			}
			realRoot, err := filepath.EvalSymlinks(themesRoot)
			if err != nil {
				fatal("cannot resolve themes root: %v", err)
			}
			if !strings.HasPrefix(realDir+string(os.PathSeparator), realRoot+string(os.PathSeparator)) {
				fatal("theme path %q escapes themes directory via symlink", *theme)
			}
			return realDir
		}
	}
	fatal("theme %q not found (checked path and ~/.config/audit-loop/themes/)", *theme)
	return ""
}

func loadPrompt(themeDir, name string) string {
	if themeDir != "" {
		path := filepath.Join(themeDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			fatal("theme missing %s: %v", path, err)
		}
		return string(data)
	}
	data, err := prompts.ReadFile("prompts/" + name)
	if err != nil {
		fatal("missing embedded prompt: %s", name)
	}
	return string(data)
}

func buildAuditorPrompt(themeDir, diff, priorResponse string, branch string, round int) string {
	tmpl := loadPrompt(themeDir, "auditor.md")

	var priorCtx string
	if priorResponse != "" {
		priorCtx = fmt.Sprintf(`## Prior Round Context

The implementer addressed your previous findings. Here is their response:

%s

If they rejected a finding with valid reasoning, do not re-flag it.
If their reasoning is wrong, escalate with stronger justification.`, priorResponse)
	}

	grounding := extractEnvironmentFacts()

	r := strings.NewReplacer(
		"{{CONTENT}}", diff,
		"{{DIFF}}", diff,
		"{{PRIOR_RESPONSE}}", priorCtx,
		"{{GROUNDING}}", grounding,
		"{{BRANCH}}", branch,
		"{{BASE}}", *base,
		"{{ROUND}}", fmt.Sprintf("%d", round),
	)
	return r.Replace(tmpl)
}

func buildAddresserPrompt(themeDir, findings string, branch string, round int) string {
	tmpl := loadPrompt(themeDir, "addresser.md")
	r := strings.NewReplacer(
		"{{FINDINGS}}", findings,
		"{{BRANCH}}", branch,
		"{{BASE}}", *base,
		"{{ROUND}}", fmt.Sprintf("%d", round),
	)
	return r.Replace(tmpl)
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

// --- Content capture ---

func captureContent(fileMode bool, paths []string) (string, error) {
	if fileMode {
		data, err := os.ReadFile(*input)
		if err != nil {
			return "", fmt.Errorf("cannot read input file: %v", err)
		}
		return string(data), nil
	}
	return captureDiff(*base, paths)
}

// --- Git helpers ---

func captureDiff(base string, paths []string) (string, error) {
	if *full {
		const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
		committed, err := gitCmd(append([]string{"diff", emptyTree, "HEAD", "--"}, paths...)...)
		if err != nil {
			return "", fmt.Errorf("failed to capture committed diff: %v", err)
		}
		working, err := gitCmd(append([]string{"diff", "HEAD", "--"}, paths...)...)
		if err != nil {
			return "", fmt.Errorf("failed to capture working diff: %v", err)
		}
		return committed + working, nil
	}
	committed, err := gitCmd(append([]string{"diff", base + "..HEAD", "--"}, paths...)...)
	if err != nil {
		return "", fmt.Errorf("failed to capture committed diff: %v", err)
	}
	working, err := gitCmd(append([]string{"diff", "HEAD", "--"}, paths...)...)
	if err != nil {
		return "", fmt.Errorf("failed to capture working diff: %v", err)
	}
	return committed + working, nil
}

func diffStats(base string, paths []string) string {
	baseRef := base + "..HEAD"
	if *full {
		baseRef = "4b825dc642cb6eb9a060e54bf8d69288fbee4904..HEAD"
	}
	var parts []string
	for _, baseArgs := range [][]string{
		{"diff", "--stat", baseRef, "--"},
		{"diff", "--stat", "HEAD", "--"},
	} {
		args := append(baseArgs, paths...)
		out, err := gitCmd(args...)
		if err != nil {
			continue
		}
		out = strings.TrimSpace(out)
		if out != "" {
			lines := strings.Split(out, "\n")
			parts = append(parts, lines[len(lines)-1])
		}
	}
	return strings.Join(parts, " | ")
}

func gitBranch() string {
	out, err := gitCmd("branch", "--show-current")
	if err != nil {
		fatal("failed to get current branch: %v", err)
	}
	return strings.TrimSpace(out)
}

func gitCmd(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// --- Environment grounding ---

// manifestFiles are project manifests that declare language/dependency versions.
var manifestFiles = []string{
	"go.mod",
	"go.sum",
	"package.json",
	"Cargo.toml",
	"pyproject.toml",
	"requirements.txt",
	"pom.xml",
	"build.gradle",
	"Gemfile",
}

// extractEnvironmentFacts reads manifest files from the project root and returns
// an authoritative context block for the auditor. This prevents false positives
// caused by stale training data (e.g., flagging a valid Go version as non-existent).
func extractEnvironmentFacts() string {
	var sections []string
	for _, name := range manifestFiles {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		content := string(data)
		// For large files (go.sum, lock files), truncate to keep prompt reasonable
		if len(content) > 4096 {
			content = content[:4096] + "\n... (truncated)\n"
		}
		sections = append(sections, fmt.Sprintf("// %s\n%s", name, content))
	}
	if len(sections) == 0 {
		return ""
	}
	return "## Environment (authoritative — do not contradict)\nThese are runtime-extracted project facts. If your training data conflicts with them, your training data is wrong.\n\n" +
		strings.Join(sections, "\n\n")
}

// --- Env/config helpers ---

func loadEnvDefaults() {
	if v := os.Getenv("AUDIT_MAX_ROUNDS"); v != "" {
		fmt.Sscanf(v, "%d", maxRounds)
	}
	if v := os.Getenv("AUDIT_BASE"); v != "" {
		*base = v
	}
	if v := os.Getenv("AUDIT_INPUT"); v != "" {
		*input = v
	}
	if v := os.Getenv("AUDIT_MODEL"); v != "" {
		*model = v
	}
	if v := os.Getenv("AUDIT_AUDITOR"); v != "" {
		*auditor = v
	}
	if v := os.Getenv("AUDIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*timeout = d
		}
	}
	if v := os.Getenv("AUDIT_AGENT"); v != "" {
		*agent = v
	}
	if v := os.Getenv("AUDIT_THEME"); v != "" {
		*theme = v
	}
	if v := os.Getenv("AUDIT_LOG_DIR"); v != "" {
		*logDir = v
	}
}

func preflightTools() error {
	for _, cmd := range []string{*auditor, "kiro-cli"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("%s not found in PATH", cmd)
		}
	}
	return nil
}

func preflight() error {
	if err := preflightTools(); err != nil {
		return err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git not found in PATH")
	}
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("not inside a git repository")
	}
	if !*full {
		if _, err := exec.Command("git", "rev-parse", "--verify", *base).Output(); err != nil {
			return fmt.Errorf("base ref %q not found", *base)
		}
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
