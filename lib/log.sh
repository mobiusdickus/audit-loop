#!/usr/bin/env bash
# log.sh — write structured markdown audit log

LOG_DIR="${AUDIT_LOG_DIR:-.audit/reviews}"
LOG_FILE=""

log_init() {
  local branch="$1" base="$2" max_rounds="$3"
  local timestamp
  timestamp=$(date +"%Y%m%d-%H%M%S")

  mkdir -p "$LOG_DIR"
  LOG_FILE="${LOG_DIR}/${timestamp}.md"

  cat > "$LOG_FILE" << EOF
# Audit Review — $(date -u +"%Y-%m-%dT%H:%M:%S%z")

## Meta
- **Branch**: ${branch}
- **Base**: ${base}
- **Max rounds**: ${max_rounds}
EOF
}

log_round_audit() {
  local round="$1" verdict="$2" findings="$3"

  cat >> "$LOG_FILE" << EOF

---

## Round ${round}

### Audit (Claude)
**Verdict**: ${verdict}

${findings}
EOF
}

log_round_response() {
  local response="$1"

  cat >> "$LOG_FILE" << EOF

### Response (Kiro)
${response}
EOF
}

log_finish() {
  local rounds="$1" max_rounds="$2" verdict="$3" duration="$4" diff_stats="$5"

  # Prepend final stats to meta section
  sed -i '' "/^- \*\*Max rounds\*\*:/a\\
- **Rounds**: ${rounds}/${max_rounds}\\
- **Verdict**: ${verdict}\\
- **Duration**: ${duration}s\\
- **Diff stats**: ${diff_stats}
" "$LOG_FILE"

  cat >> "$LOG_FILE" << EOF

---

## Result
EOF

  if [ "$verdict" = "APPROVED" ]; then
    echo "✅ Approved after ${rounds} round(s)." >> "$LOG_FILE"
  else
    echo "⚠️ Max rounds exhausted. Unresolved findings remain." >> "$LOG_FILE"
  fi
}

# Print to terminal with prefix
log_info() {
  echo -e "\033[36m[audit-loop]\033[0m $*"
}

log_error() {
  echo -e "\033[31m[audit-loop]\033[0m $*" >&2
}
