#!/usr/bin/env bash
# Regenerates docs/cli.md from the CLI's own help (run after changing help text).
set -euo pipefail
cd "$(dirname "$0")/.."
go build -o /tmp/pulse-docs ./cmd/pulse
{
  echo "# CLI reference"
  echo
  echo "Generated from \`pulse help <command>\` by scripts/gen-cli-docs.sh."
  echo
  echo '```'
  /tmp/pulse-docs help 2>&1 || true
  echo '```'
  for c in run push watch ls show open stop rm alert mute action do apply trust ask inbox answer remote skill status; do
    echo
    echo "## pulse $c"
    echo
    echo '```'
    /tmp/pulse-docs help "$c"
    echo '```'
  done
} > docs/cli.md
rm -f /tmp/pulse-docs
echo "wrote docs/cli.md"
