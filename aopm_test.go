package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fakeCLI = `#!/bin/sh
if [ -n "$FAKE_LOG" ]; then printf '%s\n' "$@" >> "$FAKE_LOG"; printf '\n' >> "$FAKE_LOG"; fi
if [ -n "$FAKE_ENV_LOG" ]; then printf '%s\n' "$OPENCODE_PERMISSION" > "$FAKE_ENV_LOG"; fi
if [ "$1" = "--version" ]; then printf '\033[31mfake-1.2.3\033[0m\n'; exit 0; fi
if [ "$1" = "login" ] || [ "$1" = "auth" ] || [ "$1" = "models" ]; then printf 'logged in\n'; exit 0; fi
if [ "$FAKE_MODE" = "big" ]; then head -c 33554433 /dev/zero; exit 0; fi
if [ "$FAKE_MODE" = "fail" ]; then printf '%2048s\n' error >&2; exit 7; fi
if [ "$FAKE_MODE" = "sleep" ]; then (sleep 30) & echo $! > "$FAKE_CHILD"; wait; fi
case "$FAKE_KIND" in
antigravity) if [ "$FAKE_MODE" = denied ]; then printf '{"conversation_id":"agy-2","status":"SUCCESS","response":"%s","denied_actions":[{"action":"command"}]}\n' "$FAKE_RESPONSE"; exit 0; fi; printf '%s\n' '{"conversation_id":"agy-1","status":"SUCCESS","response":"ok"}' ;;
codex) printf '%s\n' '{"type":"thread.started","thread_id":"codex-1"}' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' '{"type":"turn.completed"}' ;;
copilot) printf 'ok\n' ;;
claude) printf '%s\n' '{"result":"ok","is_error":false,"session_id":"claude-1","subtype":"success"}' ;;
opencode) printf '%s\n' '{"sessionID":"open-1","type":"text","part":{"text":"ok"}}' ;;
esac
`

func fakeBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(p, []byte(fakeCLI), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func callJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func captureStdout(t *testing.T, f func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	var b []byte
	go func() { b, err = io.ReadAll(r); close(done) }()
	f()
	_ = w.Close()
	os.Stdout = old
	<-done
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackendAskContinueAndPersistence(t *testing.T) {
	ws := t.TempDir()
	bin := fakeBinary(t)
	for _, backend := range backendNames {
		t.Run(backend, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			t.Setenv("FAKE_KIND", backend)
			t.Setenv("FAKE_MODE", "")
			logPath := filepath.Join(t.TempDir(), "argv")
			t.Setenv("FAKE_LOG", logPath)
			for _, env := range []string{"AGY_BIN", "CODEX_BIN", "COPILOT_BIN", "CLAUDE_BIN", "OPENCODE_BIN"} {
				t.Setenv(env, "")
			}
			envName := map[string]string{"antigravity": "AGY_BIN", "codex": "CODEX_BIN", "copilot": "COPILOT_BIN", "claude": "CLAUDE_BIN", "opencode": "OPENCODE_BIN"}[backend]
			t.Setenv(envName, bin)
			canonical, err := normalizeWorkspace(ws)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"prompt": "hello", "workspace": ws, "timeout_s": 5, "sandbox": "workspace-write"}
			answer, err := executeTool(context.Background(), backend+"_ask", callJSON(t, args))
			if err != nil || answer != "ok" {
				t.Fatalf("ask: %q, %v", answer, err)
			}
			logBytes, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			argv := string(logBytes)
			for _, want := range map[string][]string{
				"antigravity": {"--print-timeout", "--disable-slash-commands", "--sandbox\n--settings\n" + antigravitySettings("workspace-write"), "-p", "hello"},
				"codex":       {"--json", "--skip-git-repo-check", "-s", "workspace-write", "-C", canonical, "--", "-"},
				"copilot":     {"--session-id", "-C", canonical, "--allow-all-tools", "--model", "auto", "-s", "-p", "hello"},
				"claude":      {"--output-format", "json", "--permission-mode", "acceptEdits", "-p"},
				"opencode":    {"run", "--format", "json", "--dir", canonical, "--", "hello"},
			}[backend] {
				if !strings.Contains(argv, want) {
					t.Fatalf("%s argv missing %q:\n%s", backend, want, argv)
				}
			}
			p := sessionPath(backend, canonical)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var pin sessionPin
			if json.Unmarshal(b, &pin) != nil || pin.Session == "" {
				t.Fatalf("bad pin: %s", b)
			}
			if pin.Sandbox != "workspace-write" {
				t.Fatalf("sandbox not persisted: %+v", pin)
			}
			answer, err = executeTool(context.Background(), backend+"_continue", callJSON(t, map[string]any{"prompt": "again", "workspace": ws, "timeout_s": 5}))
			if err != nil || answer != "ok" {
				t.Fatalf("continue: %q, %v", answer, err)
			}
			continued, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			cargv := strings.TrimPrefix(string(continued), argv)
			want := map[string]string{
				"antigravity": "--settings\n" + antigravitySettings("workspace-write") + "\n--disable-slash-commands\n--mode\naccept-edits\n--output-format\njson\n--conversation\nagy-1",
				"codex":       "sandbox_mode=workspace-write\n--\ncodex-1\n-",
				"copilot":     "--session-id\n" + pin.Session,
				"claude":      "--resume\nclaude-1",
				"opencode":    "-s\nopen-1",
			}[backend]
			if !strings.Contains(cargv, want) {
				t.Fatalf("%s continue argv missing %q:\n%s", backend, want, cargv)
			}
		})
	}
}

func TestArgumentAndWorkspaceValidation(t *testing.T) {
	if _, err := parseToolArgs("codex_ask", callJSON(t, map[string]any{"prompt": "x", "workspace": "relative"})); err != nil {
		t.Fatal(err)
	}
	if _, err := parseToolArgs("codex_ask", callJSON(t, map[string]any{"prompt": " ", "workspace": "/tmp"})); err == nil {
		t.Fatal("blank prompt accepted")
	}
	if _, err := parseToolArgs("codex_ask", callJSON(t, map[string]any{"prompt": "x", "workspace": "/tmp", "timeout_s": 1.5})); err == nil {
		t.Fatal("float timeout accepted")
	}
	if _, err := parseToolArgs("codex_ask", callJSON(t, map[string]any{"prompt": "x", "workspace": "/tmp", "extra": true})); err == nil {
		t.Fatal("extra argument accepted")
	}
	if _, err := parseToolArgs("antigravity_ask", callJSON(t, map[string]any{"prompt": "x", "workspace": "/tmp", "schema": "[]"})); err == nil {
		t.Fatal("array schema accepted")
	}
	if _, err := parseToolArgs("opencode_ask", callJSON(t, map[string]any{"prompt": "x", "workspace": "/tmp", "effort": "high"})); err == nil {
		t.Fatal("opencode effort accepted")
	}
	if _, err := normalizeWorkspace("relative"); err == nil {
		t.Fatal("relative workspace accepted")
	}
}

func TestProtocolIDsErrorsAndConcurrency(t *testing.T) {
	s := newServer()
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"1","method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"missing"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`not json`,
		`[]`,
	}, "\n") + "\n"
	out := captureStdout(t, func() { s.serve(strings.NewReader(input)); s.shutdown() })
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 8 {
		t.Fatalf("responses=%d\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], `"id":1`) || !strings.Contains(lines[1], `"id":"1"`) {
		t.Fatalf("ids not preserved: %s", out)
	}
	if !strings.Contains(lines[2], `"protocolVersion":"2024-11-05"`) {
		t.Fatal(string(out))
	}
	all := string(out)
	for _, code := range []string{"-32601", "-32602", "-32700", "-32600"} {
		if !strings.Contains(all, code) {
			t.Fatalf("missing %s: %s", code, out)
		}
	}
}

func TestCancellationKillsGrandchildAndTimeout(t *testing.T) {
	bin := fakeBinary(t)
	ws := t.TempDir()
	child := filepath.Join(t.TempDir(), "child")
	t.Setenv("COPILOT_BIN", bin)
	t.Setenv("FAKE_KIND", "copilot")
	t.Setenv("FAKE_MODE", "sleep")
	t.Setenv("FAKE_CHILD", child)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newServer()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	s.handleLine(callJSON(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": map[string]any{"name": "copilot_ask", "arguments": map[string]any{"prompt": "wait", "workspace": ws, "timeout_s": 5}}}))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(child); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.handleLine(callJSON(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 9}}))
	s.shutdown()
	_ = w.Close()
	os.Stdout = old
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if len(got) != 0 {
		t.Fatalf("cancelled request responded: %s", got)
	}
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		state, _ := exec.Command("ps", "-o", "state=", "-p", fmt.Sprint(pid)).Output()
		if st := strings.TrimSpace(string(state)); st != "" && !strings.HasPrefix(st, "Z") {
			t.Fatalf("grandchild still alive: %d (%s)", pid, state)
		}
	}

	t.Setenv("FAKE_MODE", "sleep")
	_, err = executeTool(context.Background(), "copilot_ask", callJSON(t, map[string]any{"prompt": "timeout", "workspace": ws, "timeout_s": 1}))
	if err == nil || !strings.Contains(err.Error(), "Raise timeout_s") {
		t.Fatalf("timeout error: %v", err)
	}
	_ = r
}

func TestCorruptSessionAndStatus(t *testing.T) {
	ws := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	canonical, err := normalizeWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := sessionPath("codex", canonical)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(context.Background(), "codex_continue", callJSON(t, map[string]any{"prompt": "x", "workspace": ws})); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt session: %v", err)
	}

	bin := fakeBinary(t)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("FAKE_KIND", "codex")
	status, err := executeTool(context.Background(), "codex_status", callJSON(t, map[string]any{}))
	if err != nil || !strings.Contains(status, bin) || !strings.Contains(status, "fake-1.2.3") || strings.Contains(status, "\x1b") {
		t.Fatalf("status: %q, %v", status, err)
	}
}

func TestBackendParsersAndOutputLimits(t *testing.T) {
	answer, session, detail, failed, complete := parseCodex("{\"type\":\"thread.started\",\"thread_id\":\"t\"}\n{\"type\":\"error\",\"message\":\"temporary\"}\n")
	if answer != "" || session != "t" || detail != "temporary" || failed || complete {
		t.Fatalf("codex parser: %q %q %q %v %v", answer, session, detail, failed, complete)
	}
	_, session, detail, rejected := parseOpenCode("{\"sessionID\":\"s\",\"type\":\"error\",\"error\":{\"data\":{\"message\":\"bad\"}}}\n")
	if session != "s" || detail != "bad" || rejected {
		t.Fatalf("opencode parser: %q %q %v", session, detail, rejected)
	}
	obj, ok := findJSONObject("banner {\"response\":\"ok\"} tail", "response")
	if !ok || rawString(obj["response"]) != "ok" {
		t.Fatal("antigravity parser")
	}

	bin := fakeBinary(t)
	ws := t.TempDir()
	t.Setenv("COPILOT_BIN", bin)
	t.Setenv("FAKE_KIND", "copilot")
	t.Setenv("FAKE_MODE", "big")
	if _, err := runCopilot(context.Background(), runOptions{Binary: bin, Workspace: ws, Prompt: "x", Session: "s", Timeout: 5}); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("stdout cap: %v", err)
	}
	t.Setenv("FAKE_MODE", "fail")
	if _, err := runCopilot(context.Background(), runOptions{Binary: bin, Workspace: ws, Prompt: "x", Session: "s", Timeout: 5}); err == nil || !strings.Contains(err.Error(), "exited 7") || !strings.Contains(err.Error(), "error") {
		t.Fatalf("exit error: %v", err)
	}
}

func TestToolCallAcceptsMeta(t *testing.T) {
	name, _, err := decodeToolCall(json.RawMessage(`{"name":"codex_status","arguments":{},"_meta":{"progressToken":1}}`))
	if err != nil || name != "codex_status" {
		t.Fatalf("_meta rejected: %q, %v", name, err)
	}
}

func TestAntigravityPolicy(t *testing.T) {
	for sandbox, wantWrite := range map[string]bool{"read-only": false, "": false, "workspace-write": true} {
		var s struct {
			AgentMode               *string
			ToolPermission          string
			AllowNonWorkspaceAccess *bool
			Permissions             struct{ Deny []string }
		}
		if err := json.Unmarshal([]byte(antigravitySettings(sandbox)), &s); err != nil {
			t.Fatal(err)
		}
		deny := " " + strings.Join(s.Permissions.Deny, " ")
		if s.AgentMode == nil || *s.AgentMode != "" || s.ToolPermission != "proceed-in-sandbox" || s.AllowNonWorkspaceAccess == nil || *s.AllowNonWorkspaceAccess {
			t.Fatalf("%q: %+v", sandbox, s)
		}
		if !strings.Contains(deny, " mcp(*)") || !strings.Contains(deny, " unsandboxed(*)") {
			t.Fatalf("%q: escape routes open: %s", sandbox, deny)
		}
		if strings.Contains(deny, " write_file(*)") == wantWrite || strings.Contains(deny, " command(*)") == wantWrite {
			t.Fatalf("%q: %s", sandbox, deny)
		}
	}
	// 拒否で止まっても、作業途中のセッションを再開できる。
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("FAKE_KIND", "antigravity")
	t.Setenv("FAKE_MODE", "denied")
	t.Setenv("AGY_BIN", fakeBinary(t))
	ws, err := normalizeWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(context.Background(), "antigravity_ask", callJSON(t, map[string]any{"prompt": "x", "workspace": ws, "sandbox": "workspace-write"})); err == nil || !strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("denied: %v", err)
	}
	if p, _, err := loadSession("antigravity", ws); err != nil || p.Session != "agy-2" || p.Sandbox != "workspace-write" {
		t.Fatalf("%+v %v", p, err)
	}
	t.Setenv("FAKE_RESPONSE", "partial")
	logPath := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_LOG", logPath)
	// planと既定は読み取り専用で、編集の自動承認を付けない。
	for _, extra := range []map[string]any{{}, {"plan": true, "sandbox": "workspace-write"}} {
		extra["prompt"], extra["workspace"] = "x", ws
		answer, err := executeTool(context.Background(), "antigravity_ask", callJSON(t, extra))
		if err != nil || !strings.Contains(answer, "partial") || !strings.Contains(answer, "policy denied") {
			t.Fatalf("%q %v", answer, err)
		}
	}
	if p, _, err := loadSession("antigravity", ws); err != nil || p.Sandbox != "read-only" {
		t.Fatalf("plan pin: %+v %v", p, err)
	}
	argv, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "accept-edits") || strings.Contains(string(argv), "[Sandbox:") || strings.Count(string(argv), antigravitySettings("read-only")) != 2 {
		t.Fatalf("argv: %s", argv)
	}
	// サンドボックス対応前に保存された記録は read-only として再開する。
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := saveSession("antigravity", "/ws", sessionPin{Session: "old", Sandbox: "none"}); err != nil {
		t.Fatal(err)
	}
	if p, _, err := loadSession("antigravity", "/ws"); err != nil || p.Sandbox != "read-only" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPlanSlashGuard(t *testing.T) {
	t.Setenv("FAKE_KIND", "antigravity")
	bin, ws := fakeBinary(t), t.TempDir()
	for prompt, ok := range map[string]bool{"/goal x": false, " /goal": false, "/etc/hosts とは": true, "hi": true} {
		_, err := runAntigravity(context.Background(), runOptions{Binary: bin, Workspace: ws, Prompt: prompt, Plan: true, Timeout: 5})
		if (err == nil) != ok {
			t.Fatalf("%q: %v", prompt, err)
		}
	}
}

func TestEOFShutdown(t *testing.T) {
	bin := fakeBinary(t)
	ws := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("COPILOT_BIN", bin)
	t.Setenv("FAKE_KIND", "copilot")
	t.Setenv("FAKE_MODE", "sleep")
	s := newServer()
	out := captureStdout(t, func() {
		s.serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"copilot_ask","arguments":{"prompt":"wait","workspace":"` + ws + `","timeout_s":5}}}` + "\n"))
		s.shutdown()
	})
	if len(out) != 0 {
		t.Fatalf("EOF response: %s", out)
	}
}
