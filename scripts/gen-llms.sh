#!/usr/bin/env sh
# Regenerate docs/llms.txt and docs/llms-full.txt from the fragua binary.
#
# Usage:
#   scripts/gen-llms.sh
#
# The content is derived from internal/script/usage.go and agent/AGENTS.md, so
# run this after changing a verb or the agent guide. TestDocsAreCurrent in
# internal/llms fails when the checked-in files are stale.

set -eu

cd "$(dirname "$0")/.."
exec go run ./cmd/gen-llms docs
