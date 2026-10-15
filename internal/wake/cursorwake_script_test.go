package wake

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests run examples/cursor-cli/cursor-wake.sh with a fake
// cursor-agent, on the harness in copilotwake_script_test.go.

// fakeCursorAgent answers --version the way cursor-agent does. A run
// records its argv (NUL separated), its working directory and the
// workspace's .cursor/mcp.json and .cursor/cli.json, then prints Cursor's
// JSON result with FAKE_SID as its session id.
const fakeCursorAgent = `#!/bin/sh
if [ "${1:-}" = --version ]; then
  echo "${FAKE_VERSION:-2026.10.14-0f9edde}"
  exit 0
fi
n=$(ls "$ARGV_DIR" 2>/dev/null | grep -c "^run\\." || true)
for a in "$@"; do printf '%s\0' "$a"; done > "$ARGV_DIR/run.$n"
pwd -P > "$ARGV_DIR/cwd.$n"
ws=.
prev=
for a in "$@"; do
  [ "$prev" = --workspace ] && ws=$a
  prev=$a
done
cp "$ws/.cursor/mcp.json" "$ARGV_DIR/mcp.$n" 2>/dev/null || true
cp "$ws/.cursor/cli.json" "$ARGV_DIR/cli.$n" 2>/dev/null || true
[ -z "${FAKE_STDERR:-}" ] || echo "$FAKE_STDERR" >&2
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "${FAKE_SID:-0f6c2a1e-9b7d-4c3a-8e5f-1a2b3c4d5e6f}"
exit "${FAKE_EXIT:-0}"
`

func newCursorWake(t *testing.T) *cliWake {
	t.Helper()
	return newCLIWake(t, "cursor-cli/cursor-wake.sh", "CURSOR_AGENT_BIN", "cursor-agent", fakeCursorAgent, "cursor-cli")
}

// The wake runs cursor-agent headless with JSON output in its workspace,
// approving the workspace's MCP servers (which the wake wrote: only
// agent-tincan) and permitting exactly the agent-tincan tools in the
// workspace's cli.json: never --force or --yolo. The session id Cursor
// reports is recorded.
func TestCursorWakeRunsHeadlessWithTincanToolsOnly(t *testing.T) {
	h := newCursorWake(t)
	r := h.run()
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("cursor-agent ran %d times, want 1\nstderr:\n%s", len(runs), r.stderr)
	}
	a := runs[0]
	for _, f := range []string{"-p", "--approve-mcps", "--trust"} {
		if !slices.Contains(a.argv, f) {
			t.Errorf("argv lacks %s: %q", f, a.argv)
		}
	}
	for _, f := range []string{"--force", "-f", "--yolo"} {
		if slices.Contains(a.argv, f) {
			t.Errorf("argv has %s: %q", f, a.argv)
		}
	}
	if got := a.flagValues("--output-format"); len(got) != 1 || got[0] != "json" {
		t.Errorf("--output-format = %q, want json", got)
	}
	if got := a.flagValues("--workspace"); len(got) != 1 || got[0] != canonical(t, h.workdir) {
		t.Errorf("--workspace = %q, want the workdir", got)
	}
	checkDrainPrompt(t, a.prompt())
	if got := strings.TrimSpace(h.file("cwd.0")); got != canonical(t, h.workdir) {
		t.Fatalf("cursor-agent ran in %q, want the workdir", got)
	}
	h.tincanServer(h.file("mcp.0"))
	var cli struct {
		Permissions struct{ Allow, Deny []string } `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(h.file("cli.0")), &cli); err != nil {
		t.Fatalf("cli.json: %v\n%s", err, h.file("cli.0"))
	}
	var want []string
	for _, tool := range tincanTools {
		want = append(want, "Mcp(agent-tincan:"+tool+")")
	}
	if !slices.Equal(slices.Sorted(slices.Values(cli.Permissions.Allow)), slices.Sorted(slices.Values(want))) || len(cli.Permissions.Deny) != 0 {
		t.Errorf("cli.json permissions = %+v, want allow exactly %q", cli.Permissions, want)
	}
	h.sessionList("0f6c2a1e-9b7d-4c3a-8e5f-1a2b3c4d5e6f")
	if !strings.Contains(r.stdout, `"result":"done"`) {
		t.Fatalf("cursor-agent's output not passed to the listener log:\n%s", r.stdout)
	}
}

// Cursor loads the owner's ~/.cursor/mcp.json too, and --approve-mcps
// approves every server in it, so any server there other than the
// workspace's agent-tincan stops the wake. So does a workspace hooks.json,
// whose commands would run unattended.
func TestCursorWakeIdentityCheck(t *testing.T) {
	for _, tc := range []struct{ name, file, body, want string }{
		{"an unvetted server", ".cursor/mcp.json", `{"mcpServers":{"files":{"command":"npx","args":["fs"]}}}`, "did not allow: files"},
		{"the owner's agent-tincan", ".cursor/mcp.json", `{"mcpServers":{"agent-tincan":{"command":"tincan","args":["mcp"],"env":{"TINCAN_CONFIG":"/Users/owner/.config/tincan/config.json"}}}}`, "2 agent-tincan servers"},
		{"a workspace hook", "work-hooks", `{"version":1,"hooks":{}}`, "hooks.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCursorWake(t)
			if tc.file == "work-hooks" {
				h.write(filepath.Join(h.workdir, ".cursor", "hooks.json"), tc.body)
			} else {
				h.write(filepath.Join(h.home, tc.file), tc.body)
			}
			h.refused(h.run(), tc.want)
		})
	}
}
