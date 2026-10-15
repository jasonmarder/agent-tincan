#!/bin/sh
# Wake for the cursor-cli teammate: Cursor's CLI agent (cursor-agent), run
# headless by "tincan listen --exec".
#
# cursor-agent has no daemon, so an idle cursor-cli needs something to
# start a fresh run when teammates' requests are waiting. Copy this script
# and examples/lib/tincan-wake-lib.sh from the agent-tincan repo (they are
# not in the release downloads) into one folder you keep, chmod +x this one,
# and run:
#
#   TINCAN_CONFIG=~/.config/tincan/cursor-cli.json tincan listen --exec ~/bin/cursor-wake.sh
#
# cursor-agent runs with the owner's own login, in a workspace of its own
# (<wake home>/work). Before every run the wake rewrites the workspace's
# .cursor/mcp.json, holding only the agent-tincan server with this
# teammate's TINCAN_CONFIG, and its .cursor/cli.json, which permits
# exactly the Agent Tincan tools (Mcp(agent-tincan:<tool>)). The run gets
# --approve-mcps and --trust so the workspace server loads in -p mode, and
# never --force or --yolo: any other tool call needs a permission the run
# does not have. Cursor also loads the owner's ~/.cursor/mcp.json, and
# --approve-mcps approves every server there, so any server in it stops
# the wake unless TINCAN_WAKE_ALLOWED_SERVERS lists it. A hooks.json in the
# workspace stops it too.
#
# Hangs are handled by the library's run timeout (TINCAN_WAKE_TIMEOUT).
#
# Each run's session id, from Cursor's JSON output, is recorded with the
# workdir in a private list beside TINCAN_CONFIG (<name>.wake-sessions), so
# the history agent leaves wake runs out. See docs/adapters/cursor-cli.md.
#
# Settings, read from the listener's environment (the library's settings,
# TINCAN_WAKE_*, are listed in tincan-wake-lib.sh):
#
#   TINCAN_CONFIG                 this teammate's tincan config (default
#                                 ~/.config/tincan/cursor-cli.json)
#   TINCAN_CURSOR_WAKE_HOME       the wake home (default <name>.wake beside
#                                 TINCAN_CONFIG)
#   TINCAN_CURSOR_WORKDIR         the workspace cursor-agent runs in
#                                 (default <wake home>/work)
#   TINCAN_CURSOR_VERSION_PATTERN what "cursor-agent --version" must print
#                                 (default a YYYY.MM.DD-<commit> version)
#   TINCAN_CURSOR_JSON_TOOL       jq or python3, to read the MCP configs
#                                 (default jq when on PATH)
#   CURSOR_AGENT_BIN              the cursor-agent binary (default
#                                 cursor-agent)
#   CURSOR_API_KEY                a Cursor API key, instead of the owner's
#                                 login
#   TINCAN_WAKE_STATE_DIR         as in the library, but by default
#                                 <name>.wake-state beside TINCAN_CONFIG
set -eu

: "${TINCAN_CONFIG:=$HOME/.config/tincan/cursor-cli.json}"
# shellcheck disable=SC2088 # a literal ~/ prefix, which tincan expands
case $TINCAN_CONFIG in "~/"*) TINCAN_CONFIG=$HOME/${TINCAN_CONFIG#"~/"} ;; esac
case $TINCAN_CONFIG in /*) ;; *) TINCAN_CONFIG=$(pwd -P)/$TINCAN_CONFIG ;; esac
export TINCAN_CONFIG

# The library sits next to this script, or in the repo's examples/lib.
here=$(dirname -- "$0")
if [ -z "${TINCAN_WAKE_LIB:-}" ]; then
  TINCAN_WAKE_LIB=$here/tincan-wake-lib.sh
  [ -f "$TINCAN_WAKE_LIB" ] || TINCAN_WAKE_LIB=$here/../lib/tincan-wake-lib.sh
fi
if [ ! -f "$TINCAN_WAKE_LIB" ]; then
  echo "cursor-cli-wake: tincan-wake-lib.sh not found; copy examples/lib/tincan-wake-lib.sh next to this script" >&2
  exit 2
fi
# shellcheck source=SCRIPTDIR/../lib/tincan-wake-lib.sh
. "$TINCAN_WAKE_LIB"

cfg_dir=$(dirname -- "$TINCAN_CONFIG")
cfg_name=$(basename -- "$TINCAN_CONFIG" .json)
WAKE_HOME=${TINCAN_CURSOR_WAKE_HOME:-$cfg_dir/$cfg_name.wake}
WAKE_HOME=${WAKE_HOME%/}
WORKDIR=${TINCAN_CURSOR_WORKDIR:-$WAKE_HOME/work}
SESSIONS=$cfg_dir/$cfg_name.wake-sessions
BIN=${CURSOR_AGENT_BIN:-cursor-agent}
# cursor-agent prints its build, 2026.10.14-0f9edde. (Braces cannot sit
# inside a ${...:-default}, so the default is set apart.)
VERSION_PATTERN='^[0-9][0-9][0-9][0-9][.][0-9]+[.][0-9]+-[0-9a-f]+'
VERSION_PATTERN=${TINCAN_CURSOR_VERSION_PATTERN:-$VERSION_PATTERN}
# The Agent Tincan tools (internal/mcpserver), the only ones a run may use.
TINCAN_TOOLS="ask get_reply check_inbox claim progress reply answer cancel list_agents onboard get_attachment trace search"

: "${TINCAN_WAKE_STATE_DIR:=$cfg_dir/$cfg_name.wake-state}"
case $TINCAN_WAKE_STATE_DIR in /*) ;; *) TINCAN_WAKE_STATE_DIR=$(pwd -P)/$TINCAN_WAKE_STATE_DIR ;; esac

tincan_wake_init cursor-cli
tincan_wake_begin

if ! { mkdir -p "$WORKDIR/.cursor" && chmod 700 "$WAKE_HOME"; } 2>/dev/null ||
  ! workdir_c=$(tincan_wake_canonical_dir "$WORKDIR"); then
  tincan_wake_refuse "cannot create the workdir $WORKDIR (set TINCAN_CURSOR_WORKDIR to an absolute path this user can write)"
fi

tincan_wake_check_binary "$BIN" "$VERSION_PATTERN"

JSON_TOOL=$(tincan_wake_json_tool "${TINCAN_CURSOR_JSON_TOOL:-}")
if [ -z "$JSON_TOOL" ]; then
  tincan_wake_refuse "reading Cursor's MCP config needs jq or python3 on the listener's PATH (or TINCAN_CURSOR_JSON_TOOL names one that is missing)"
fi

# A workspace hook would run its commands unattended.
if [ -e "$workdir_c/.cursor/hooks.json" ] || [ -L "$workdir_c/.cursor/hooks.json" ]; then
  tincan_wake_refuse "found $workdir_c/.cursor/hooks.json; its hooks would run unattended in every wake, so remove it"
fi

# write_file FILE: writes stdin to FILE through a temporary file.
write_file() {
  if ! { cat >"$1.$$" && mv -f "$1.$$" "$1"; } 2>/dev/null; then
    rm -f "$1.$$"
    tincan_wake_refuse "cannot write $1"
  fi
}

# The workspace's MCP config, rewritten before every run: only agent-tincan.
tincan_path=$(command -v "${TINCAN_BIN:-tincan}" 2>/dev/null) || tincan_wake_refuse "${TINCAN_BIN:-tincan} is not installed or not on PATH"
case $tincan_path in /*) ;; *) tincan_wake_refuse "${TINCAN_BIN:-tincan} is not a program on PATH; set TINCAN_BIN to the tincan binary's path" ;; esac
printf '{"mcpServers":{"agent-tincan":{"command":%s,"args":["mcp"],"env":{"TINCAN_CONFIG":%s}}}}\n' \
  "$(tincan_wake_json_string "$tincan_path")" "$(tincan_wake_json_string "$TINCAN_CONFIG")" |
  write_file "$workdir_c/.cursor/mcp.json"

# Cursor loads the owner's ~/.cursor/mcp.json and the workspace's.
# shellcheck disable=SC2329 # run by tincan_wake_check_identity
cursor_mcp_list() {
  tincan_wake_mcp_json "$JSON_TOOL" "$HOME/.cursor/mcp.json" "$workdir_c/.cursor/mcp.json"
}
tincan_wake_check_identity cursor_mcp_list

# The workspace's permissions, rewritten before every run: exactly the
# Agent Tincan tools.
{
  printf '{"permissions":{"allow":['
  sep=
  for tool in $TINCAN_TOOLS; do
    printf '%s%s' "$sep" "$(tincan_wake_json_string "Mcp($tincan_wake_server:$tool)")"
    sep=,
  done
  printf '],"deny":[]}}\n'
} | write_file "$workdir_c/.cursor/cli.json"

# The workdir is recorded before the run, so even a run the timeout kills
# is known to the history agent; the session id once Cursor reports it.
tincan_wake_record_session "$SESSIONS" "$workdir_c"

PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan item(s) waiting: requests from teammates, or replies to requests you sent. Call check_inbox. It shows replies to your requests first, with what you asked: finish the work that was waiting on each one. Then, for each request, handle it the way you would handle a request from your owner, and call reply with that request's id and your result. Call check_inbox again and keep going until it returns nothing waiting, so this run drains the whole inbox. If you need something from a teammate yourself, call ask; it may return before the answer does, and you do not have to wait for it: you will be woken again when the reply arrives. In this run only the Agent Tincan tools are permitted; if a request needs more (running commands, tools of other servers), say so in your reply."

state=$(CDPATH='' cd -P -- "$TINCAN_WAKE_STATE_DIR" && pwd -P)
out=$state/out.$$
err=$state/err.$$
trap 'rm -f "$out" "$err"; _tw_cleanup' EXIT
cd "$workdir_c" || tincan_wake_refuse "cannot enter the workdir $workdir_c"
status=0
tincan_wake_run "$BIN" -p --output-format json --approve-mcps --trust --workspace "$workdir_c" "$PROMPT" >"$out" 2>"$err" || status=$?
cat "$out"
cat "$err" >&2
sid=$(sed -n 's/.*"session_id": *"\([A-Za-z0-9._-]*\)".*/\1/p' "$out" | head -n 1)
[ -z "$sid" ] || tincan_wake_record_session "$SESSIONS" "$workdir_c" "$sid"
auth=
# Only cursor-agent's stderr: its stdout carries the model's words, which
# can say anything.
if [ "$status" -ne 0 ] && [ "$status" -ne 124 ] &&
  grep -Eiq 'not (logged|signed) in|authentication required|unauthenticated|unauthorized|(^|[^0-9.,])401([^0-9]|$)|(cursor-)?agent login|invalid api key|CURSOR_API_KEY' "$err"; then
  auth=1
fi
rm -f "$out" "$err"
if [ -n "$auth" ]; then
  tincan_wake_backoff "cursor-agent is not logged in or its login expired; run cursor-agent login on this machine, or check CURSOR_API_KEY in the listener's environment"
fi
exit "$status"
