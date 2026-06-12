#!/usr/bin/env bash
# parse.sh — extract verdict and findings from auditor output

# Strip ANSI escape codes from input
strip_ansi() {
  sed 's/\x1B\[[0-9;]*[a-zA-Z]//g; s/\x1B\([A-Z]//g'
}

# Extract verdict (APPROVED or NEEDS_CHANGES) from output
# Scans for first line matching the pattern, skipping preamble
parse_verdict() {
  local output="$1"
  echo "$output" | grep -m1 -oE '^(APPROVED|NEEDS_CHANGES)' || echo "UNKNOWN"
}

# Extract findings (everything after the verdict line)
parse_findings() {
  local output="$1"
  echo "$output" | sed -n '/^NEEDS_CHANGES/,$ { /^NEEDS_CHANGES/d; p; }'
}

# Extract the response table from kiro's output
parse_response_table() {
  local output="$1"
  echo "$output" | sed -n '/^|.*#.*Finding.*Decision.*Reasoning/,/^$/p'
}
