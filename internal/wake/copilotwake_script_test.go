package wake

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// These tests run examples/copilot-cli/copilot-wake.sh with a fake copilot
// that records what it was run with, the way grokwake_script_test.go does
// for grok, and a fake tincan that records operator notices. The shared
// library comes from examples/lib, where the script finds it in a repo
// checkout. cursorwake_script_test.go uses the same harness.

// fakeCopilot answers --version the way GitHub Copilot CLI does. A run
// records its argv (NUL separated), its COPILOT_HOME and COPILOT_ALLOW_ALL,
// its working directory and the --additional-mcp-config file it was
// handed, then prints the agent's answer.
const fakeCopilot = `#!/bin/sh
if [ "${1:-}" = --version ]; then
  echo "${FAKE_VERSION:-GitHub Copilot CLI 1.0.98.}"
  exit 0
fi
n=$(ls "$ARGV_DIR" 2>/dev/null | grep -c "^run\\." || true)
for a in "$@"; do printf '%s\0' "$a"; done > "$ARGV_DIR/run.$n"
printf '%s\n%s\n' "${COPILOT_HOME:-}" "${COPILOT_ALLOW_ALL:-}" > "$ARGV_DIR/env.$n"
pwd -P > "$ARGV_DIR/cwd.$n"
for a in "$@"; do
  case $a in @*) cp "${a#@}" "$ARGV_DIR/mcp.$n" ;; esac
done
[ -z "${FAKE_STDERR:-}" ] || echo "$FAKE_STDERR" >&2
echo "${FAKE_STDOUT:-done}"
exit "${FAKE_EXIT:-0}"
`

// tincanTools are the agent-tincan MCP server's tools
// (internal/mcpserver), each of which a wake allows by name.
var tincanTools = []string{"ask", "get_reply", "check_inbox", "claim", "progress", "reply", "answer", "cancel", "list_agents", "onboard", "get_attachment", "trace", "search"}

// cliWake runs one command-wake script against fake binaries.
type cliWake struct {
	t                                      *testing.T
	script, binEnv, binName                string
	dir, home, config, state, argv, notice string
	wakeHome, workdir, sessions            string
	env                                    []string
}

func newCLIWake(t *testing.T, script, binEnv, binName, fake, name string) *cliWake {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the wake scripts are POSIX sh")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "home", ".config", "tincan")
	h := &cliWake{t: t, script: script, binEnv: binEnv, binName: binName, dir: dir,
		home:     filepath.Join(dir, "home"),
		config:   filepath.Join(cfgDir, name+".json"),
		state:    filepath.Join(dir, "state"),
		argv:     filepath.Join(dir, "argv"),
		notice:   filepath.Join(dir, "notices"),
		wakeHome: filepath.Join(cfgDir, name+".wake"),
		sessions: filepath.Join(cfgDir, name+".wake-sessions"),
	}
	h.workdir = filepath.Join(h.wakeHome, "work")
	mkdirs(t, h.argv, cfgDir)
	for n, body := range map[string]string{binName: fake, "tincan": fakeTincan} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.write(h.config, `{"agent":"`+name+`","relay":"http://relay"}`)
	return h
}

func (h *cliWake) write(path, body string) {
	h.t.Helper()
	mkdirs(h.t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *cliWake) run(env ...string) grokRun {
	h.t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", h.script))
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home,
		h.binEnv + "=" + filepath.Join(h.dir, h.binName),
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_WAKE_OPERATOR=ops",
		"TINCAN_CONFIG=" + h.config,
		"ARGV_DIR=" + h.argv,
		"NOTICES=" + h.notice,
		"TINCAN_WAITING=2",
	}, h.env...)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return grokRun{err: err, stdout: stdout.String(), stderr: stderr.String()}
}

func (h *cliWake) runs() []wakeRun {
	h.t.Helper()
	var out []wakeRun
	for i := 0; ; i++ {
		raw, err := os.ReadFile(filepath.Join(h.argv, "run."+strconv.Itoa(i)))
		if err != nil {
			return out
		}
		out = append(out, wakeRun{argv: strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")})
	}
}

func (h *cliWake) file(name string) string {
	raw, _ := os.ReadFile(filepath.Join(h.argv, name))
	return string(raw)
}

// refused checks that the wake ran nothing, failed, backed off and told
// the operator once, saying want.
func (h *cliWake) refused(r grokRun, want string) {
	h.t.Helper()
	if r.err == nil {
		h.t.Fatalf("wake succeeded, want a refusal\nstderr:\n%s", r.stderr)
	}
	if n := len(h.runs()); n != 0 {
		h.t.Fatalf("the CLI ran %d times, want 0", n)
	}
	if !strings.Contains(r.stderr, want) {
		h.t.Fatalf("stderr lacks %q:\n%s", want, r.stderr)
	}
	_, err := os.Stat(filepath.Join(h.state, "backoff"))
	raw, _ := os.ReadFile(h.notice)
	if err != nil || strings.Count(string(raw), "\n") != 1 {
		h.t.Fatalf("backoff %v, notices %q; want a backoff and one notice", err == nil, raw)
	}
}

// sessionList checks that the private session list holds the workdir and
// each id.
func (h *cliWake) sessionList(ids ...string) {
	h.t.Helper()
	st, err := os.Stat(h.sessions)
	if err != nil {
		h.t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		h.t.Errorf("session list mode %v, want 0600", st.Mode().Perm())
	}
	list, _ := os.ReadFile(h.sessions)
	if !strings.Contains(string(list), "workdir "+canonical(h.t, h.workdir)+"\n") {
		h.t.Fatalf("session list lacks the workdir %s:\n%s", canonical(h.t, h.workdir), list)
	}
	for _, id := range ids {
		if !strings.Contains("\n"+string(list), "\n"+id+"\n") {
			h.t.Fatalf("session list lacks %s:\n%s", id, list)
		}
	}
}

// tincanServer checks that an MCP config file holds exactly one server,
// agent-tincan, running "<tincan> mcp" with this wake's TINCAN_CONFIG.
func (h *cliWake) tincanServer(raw string) map[string]any {
	h.t.Helper()
	var cfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		h.t.Fatalf("MCP config is not JSON: %v\n%s", err, raw)
	}
	s, ok := cfg.MCPServers["agent-tincan"]
	if len(cfg.MCPServers) != 1 || !ok {
		h.t.Fatalf("MCP config servers = %v, want only agent-tincan", cfg.MCPServers)
	}
	env, _ := s["env"].(map[string]any)
	if s["command"] != filepath.Join(h.dir, "tincan") || env["TINCAN_CONFIG"] != h.config {
		h.t.Fatalf("agent-tincan = %v, want %s with TINCAN_CONFIG %s", s, filepath.Join(h.dir, "tincan"), h.config)
	}
	if args, _ := s["args"].([]any); len(args) != 1 || args[0] != "mcp" {
		h.t.Fatalf("agent-tincan args = %v, want [mcp]", s["args"])
	}
	return s
}

func newCopilotWake(t *testing.T) *cliWake {
	t.Helper()
	return newCLIWake(t, "copilot-cli/copilot-wake.sh", "COPILOT_BIN", "copilot", fakeCopilot, "copilot-cli")
}

// The wake runs Copilot CLI headless and silent, in the workdir, with
// COPILOT_HOME in the wake home, the built-in MCP servers off, the
// agent-tincan server from a config file the wake writes, a session id it
// chose and recorded, and an allowlist of exactly the agent-tincan tools:
// never --allow-all-tools, and never COPILOT_ALLOW_ALL from the
// listener's environment.
func TestCopilotWakeRunsHeadlessWithTincanToolsOnly(t *testing.T) {
	h := newCopilotWake(t)
	r := h.run("COPILOT_ALLOW_ALL=true")
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("copilot ran %d times, want 1\nstderr:\n%s", len(runs), r.stderr)
	}
	a := runs[0]
	prompts := a.flagValues("-p")
	if len(prompts) != 1 {
		t.Fatalf("argv lacks one -p prompt: %q", a.argv)
	}
	checkDrainPrompt(t, prompts[0])
	for _, f := range []string{"-s", "--disable-builtin-mcps"} {
		if !slices.Contains(a.argv, f) {
			t.Errorf("argv lacks %s: %q", f, a.argv)
		}
	}
	for _, f := range []string{"--allow-all-tools", "--allow-all", "--yolo", "--allow-all-paths"} {
		if slices.Contains(a.argv, f) {
			t.Errorf("argv has %s: %q", f, a.argv)
		}
	}
	var want []string
	for _, tool := range tincanTools {
		want = append(want, "agent-tincan("+tool+")")
	}
	if got := a.flagValues("--allow-tool"); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("--allow-tool = %q, want exactly %q", got, want)
	}
	if cfgs := a.flagValues("--additional-mcp-config"); len(cfgs) != 1 || !strings.HasPrefix(cfgs[0], "@/") {
		t.Fatalf("--additional-mcp-config = %q, want one @<absolute file>", cfgs)
	}
	s := h.tincanServer(h.file("mcp.0"))
	if s["type"] != "local" {
		t.Errorf("agent-tincan type = %v, want local", s["type"])
	}
	env := strings.Split(h.file("env.0"), "\n")
	if env[0] != filepath.Join(h.wakeHome, ".copilot") || env[1] != "" {
		t.Fatalf("copilot ran with COPILOT_HOME %q COPILOT_ALLOW_ALL %q, want the wake home and unset", env[0], env[1])
	}
	if got := strings.TrimSpace(h.file("cwd.0")); got != canonical(t, h.workdir) {
		t.Fatalf("copilot ran in %q, want the workdir", got)
	}
	sid := a.flagValues("--session-id")
	if len(sid) != 1 || !uuidShape.MatchString(sid[0]) {
		t.Fatalf("--session-id = %q, want one lowercase UUID", sid)
	}
	h.sessionList(sid[0])
	if !strings.Contains(r.stdout, "done") {
		t.Fatalf("copilot's output not passed to the listener log:\n%s", r.stdout)
	}
}

// Any MCP server in the wake home's own mcp-config.json other than
// agent-tincan stops the wake, as does a second agent-tincan there.
func TestCopilotWakeIdentityCheck(t *testing.T) {
	for _, tc := range []struct{ name, servers, want string }{
		{"an unvetted server", `{"files":{"command":"npx","args":["fs"]}}`, "did not allow: files"},
		{"a second agent-tincan", `{"agent-tincan":{"command":"tincan","args":["mcp"],"env":{"TINCAN_CONFIG":"/Users/owner/.config/tincan/config.json"}}}`, "2 agent-tincan servers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCopilotWake(t)
			h.write(filepath.Join(h.wakeHome, ".copilot", "mcp-config.json"), `{"mcpServers":`+tc.servers+`}`)
			h.refused(h.run(), tc.want)
		})
	}
}
