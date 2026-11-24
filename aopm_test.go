package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const fakeCLI = `#!/bin/sh
if [ -n "$FAKE_LOG" ]; then printf '%s\n' "$@" >> "$FAKE_LOG"; printf '\n' >> "$FAKE_LOG"; fi
if [ -n "$FAKE_ENV_LOG" ]; then printf '%s\n' "$OPENCODE_PERMISSION" > "$FAKE_ENV_LOG"; fi
if [ "$1" = "--version" ]; then printf '\033[31mfake-1.2.3\033[0m\n'; exit 0; fi
if [ "$1" = "login" ] || [ "$1" = "auth" ] || [ "$1" = "models" ]; then printf 'logged in\n'; exit 0; fi
if [ "$1" = "app-server" ]; then
	if [ -n "$FAKE_CHILD" ]; then echo $$ > "$FAKE_CHILD"; fi
	while IFS= read -r line; do
		case "$line" in
			*'"id":1'*)
				printf '%s\n' '{"method":"codex/notification","params":{}}'
				printf '%s\n' '{"id":1,"result":{"userAgent":"codex-test"}}'
				;;
			*'"id":2'*)
				printf '%s\n' '{"method":"account/updated","params":{"email":"leak@example.com"}}'
				if [ -n "$FAKE_CODEX_QUOTA" ]; then printf '%s\n' "$FAKE_CODEX_QUOTA"; else printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":14,"windowDurationMins":300,"resetsAt":1893456000},"secondary":null}}}'; fi
				;;
		esac
	done
	exit 0
fi
if [ "$1" = "-p" ] && [ "$2" = "/usage" ]; then
	if [ "$FAKE_KIND" = "antigravity" ] && [ -n "$FAKE_AGY_QUOTA" ]; then printf '%s\n' "$FAKE_AGY_QUOTA"; exit 0; fi
	if [ "$FAKE_KIND" = "claude" ] && [ -n "$FAKE_CLAUDE_QUOTA" ]; then printf '%s\n' "$FAKE_CLAUDE_QUOTA"; exit 0; fi
fi
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

func TestQuotaParsingAndPrivacy(t *testing.T) {
	bin := fakeBinary(t)

	t.Run("antigravity", func(t *testing.T) {
		t.Setenv("FAKE_KIND", "antigravity")
		soon := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
		later := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
		payload := fmt.Sprintf(`{"email":"secret@example.com","accountId":"acc-1","org":"org-1","token":"tok-1","command":{"data":{"groups":[{"name":"Claude","buckets":[{"window":"5h","remaining_fraction":0.86,"reset_time":%q},{"window":"weekly","remaining_fraction":0.5,"reset_time":"1970-01-01T00:00:00Z"},{"window":"bad1","remaining_fraction":-0.1},{"window":"bad2","remaining_fraction":1.2},{"window":"bad3"}]},{"name":"Gemini","buckets":[{"window":"weekly","remaining_fraction":0.75,"reset_time":%q}]}]}}}`, soon, later)
		t.Setenv("FAKE_AGY_QUOTA", payload)

		got := quota(context.Background(), "antigravity", bin)
		for _, secret := range []string{"secret@example.com", "acc-1", "org-1", "tok-1", "bad1", "bad2", "bad3", "1970"} {
			if strings.Contains(got, secret) {
				t.Fatalf("unexpected content %q in %q", secret, got)
			}
		}
		if !strings.Contains(got, "Claude 5h 86% left (resets ") || !strings.Contains(got, "Claude weekly 50% left") || !strings.Contains(got, "Gemini weekly 75% left (resets ") {
			t.Fatalf("unexpected antigravity quota: %q", got)
		}
		if strings.Contains(got, "Claude weekly 50% left (resets") {
			t.Fatalf("1970 reset time should be omitted: %q", got)
		}

		// 異常値のみなら not available
		t.Setenv("FAKE_AGY_QUOTA", `{"command":{"data":{"groups":[{"name":"G","buckets":[{"window":"5h","remaining_fraction":2.0}]}]}}}`)
		if got := quota(context.Background(), "antigravity", bin); got != "not available" {
			t.Fatalf("expected not available, got %q", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		t.Setenv("FAKE_KIND", "codex")
		child := filepath.Join(t.TempDir(), "codex-app-pid")
		t.Setenv("FAKE_CHILD", child)
		soon := time.Now().Add(3 * time.Hour).Unix()
		t.Setenv("FAKE_CODEX_QUOTA", fmt.Sprintf(`{"id":2,"result":{"email":"secret@example.com","accountId":"acc-2","rateLimits":{"primary":{"usedPercent":14,"windowDurationMins":300,"resetsAt":%d},"secondary":null}}}`, soon))

		got := quota(context.Background(), "codex", bin)
		if !strings.Contains(got, "5h 86% left (resets ") {
			t.Fatalf("unexpected codex quota: %q", got)
		}
		for _, secret := range []string{"secret@example.com", "acc-2", "leak@example.com"} {
			if strings.Contains(got, secret) {
				t.Fatalf("leaked %q in %q", secret, got)
			}
		}

		// プロセスが残っていないことを確認
		data, err := os.ReadFile(child)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			state, _ := exec.Command("ps", "-o", "state=", "-p", fmt.Sprint(pid)).Output()
			if st := strings.TrimSpace(string(state)); st != "" && !strings.HasPrefix(st, "Z") {
				t.Fatalf("codex app-server still alive: %d (%s)", pid, state)
			}
		}

		// primary/secondary 両方 null なら not available
		t.Setenv("FAKE_CODEX_QUOTA", `{"id":2,"result":{"rateLimits":{"primary":null,"secondary":null}}}`)
		if got := quota(context.Background(), "codex", bin); got != "not available" {
			t.Fatalf("expected not available for null rateLimits, got %q", got)
		}
	})

	t.Run("claude", func(t *testing.T) {
		t.Setenv("FAKE_KIND", "claude")
		arrPayload := `[{"type":"system","email":"secret@example.com"},{"type":"result","accountId":"acc-3","org":"org-3","result":"Header\nCurrent session: 22% used · resets Nov 24 at 5:50pm (Asia/Tokyo)\nCurrent week (all models): 15% used · resets Dec 1 at 11:32am (Asia/Tokyo)\nuser@example.com"}]`
		t.Setenv("FAKE_CLAUDE_QUOTA", arrPayload)
		got := quota(context.Background(), "claude", bin)
		if !strings.Contains(got, "5h 78% left (resets ") || !strings.Contains(got, "weekly 85% left (resets ") {
			t.Fatalf("unexpected claude array quota: %q", got)
		}
		for _, secret := range []string{"secret@example.com", "acc-3", "org-3", "user@example.com", "Header"} {
			if strings.Contains(got, secret) {
				t.Fatalf("leaked %q in %q", secret, got)
			}
		}

		objPayload := `{"type":"result","result":"Current session: 120% used"}`
		t.Setenv("FAKE_CLAUDE_QUOTA", objPayload)
		got = quota(context.Background(), "claude", bin)
		if got != "5h 0% left" {
			t.Fatalf("expected clamped 0%%, got %q", got)
		}
	})

	t.Run("copilot_and_opencode_no_cli", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "argv")
		t.Setenv("FAKE_LOG", logPath)
		for _, backend := range []string{"copilot", "opencode"} {
			if got := quota(context.Background(), backend, bin); got != "not available" {
				t.Fatalf("%s: got %q, want not available", backend, got)
			}
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			b, _ := os.ReadFile(logPath)
			t.Fatalf("CLI was invoked unexpectedly:\n%s", b)
		}
	})
}

func TestStatusMCPAndCLIArgs(t *testing.T) {
	bin := fakeBinary(t)
	for _, env := range []string{"AGY_BIN", "CODEX_BIN", "COPILOT_BIN", "CLAUDE_BIN", "OPENCODE_BIN"} {
		t.Setenv(env, "")
	}
	t.Setenv("AGY_BIN", bin)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("CLAUDE_BIN", bin)
	t.Setenv("OPENCODE_BIN", bin)
	// copilot は未導入にして 1 つの失敗が他を止めないことも確認
	t.Setenv("COPILOT_BIN", "/nonexistent/aopm-missing-copilot")
	t.Setenv("PATH", "")

	t.Setenv("FAKE_AGY_QUOTA", `{"command":{"data":{"groups":[{"name":"","buckets":[{"window":"5h","remaining_fraction":0.9,"reset_time":"1970-01-01T00:00:00Z"}]}]}}}`)
	t.Setenv("FAKE_CODEX_QUOTA", `{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":0},"secondary":null}}}`)
	t.Setenv("FAKE_CLAUDE_QUOTA", `{"type":"result","result":"Current session: 20% used"}`)

	// MCP *_status に quota: が含まれること
	status, err := executeTool(context.Background(), "codex_status", callJSON(t, map[string]any{}))
	if err != nil || !strings.Contains(status, "quota: 5h 90% left") {
		t.Fatalf("codex_status: %q, %v", status, err)
	}

	// aopm status (非端末: ESC なし)
	var out, errOut bytes.Buffer
	handled, code := runCLIArgs([]string{"aopm", "status"}, false, false, &out, &errOut)
	if !handled || code != 0 {
		t.Fatalf("status: handled=%v code=%d stderr=%s", handled, code, errOut.String())
	}
	s := out.String()
	if strings.Contains(s, "\x1b") {
		t.Fatalf("non-TTY status must not contain ESC: %q", s)
	}
	// 順序固定で 5 バックエンドが出ること
	pos := -1
	for _, name := range backendNames {
		idx := strings.Index(s, name)
		if idx <= pos {
			t.Fatalf("order mismatch for %s in:\n%s", name, s)
		}
		pos = idx
	}
	if !strings.Contains(s, "copilot     not installed") {
		t.Fatalf("expected copilot not installed in:\n%s", s)
	}
	if !strings.Contains(s, "  quota not available") {
		t.Fatalf("expected quota not available in:\n%s", s)
	}
	if !strings.Contains(s, "not verified") {
		t.Fatalf("expected opencode not verified in:\n%s", s)
	}

	// 引数なし + TTY
	out.Reset()
	errOut.Reset()
	handled, code = runCLIArgs([]string{"aopm"}, true, false, &out, &errOut)
	if !handled || code != 0 || !strings.Contains(out.String(), "MCP server: run via an MCP client") {
		t.Fatalf("tty: handled=%v code=%d out=%s", handled, code, out.String())
	}

	// 引数なし + パイプ
	out.Reset()
	handled, code = runCLIArgs([]string{"aopm"}, false, false, &out, &errOut)
	if handled || code != 0 {
		t.Fatalf("pipe: handled=%v code=%d", handled, code)
	}

	// --version / version
	for _, arg := range []string{"--version", "version"} {
		out.Reset()
		handled, code = runCLIArgs([]string{"aopm", arg}, false, false, &out, &errOut)
		if !handled || code != 0 || strings.TrimSpace(out.String()) != "aopm "+version {
			t.Fatalf("%s: handled=%v code=%d out=%q", arg, handled, code, out.String())
		}
	}

	// --help / -h / help
	for _, arg := range []string{"--help", "-h", "help"} {
		out.Reset()
		handled, code = runCLIArgs([]string{"aopm", arg}, false, false, &out, &errOut)
		if !handled || code != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("%s: handled=%v code=%d out=%q", arg, handled, code, out.String())
		}
	}

	// 未知の引数
	out.Reset()
	errOut.Reset()
	handled, code = runCLIArgs([]string{"aopm", "unknown"}, false, false, &out, &errOut)
	if !handled || code != 2 || !strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("unknown: handled=%v code=%d err=%q", handled, code, errOut.String())
	}
}

type syncFrameWriter struct {
	mu      sync.Mutex
	frames  []string
	firstCh chan struct{}
	once    sync.Once
}

func (w *syncFrameWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.frames = append(w.frames, string(p))
	w.mu.Unlock()
	w.once.Do(func() { close(w.firstCh) })
	return len(p), nil
}

func TestLiveStatusRenderingAndCancel(t *testing.T) {
	// 遅い偽CLI: 開始シグナルファイルを作ってから少し待つ
	started := filepath.Join(t.TempDir(), "started")
	gate := filepath.Join(t.TempDir(), "gate")
	slowScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
	: > %q
	while [ ! -f %q ]; do sleep 0.02; done
	printf '\033[31mslow-1.0\t\007\033[0m\n'
	exit 0
fi
if [ "$1" = "auth" ]; then exit 0; fi
if [ "$1" = "-p" ] && [ "$2" = "/usage" ]; then
	printf '%%s\n' '{"type":"result","result":"Current session: 25%% used · resets 18:00\t\u0007"}'
	exit 0
fi
exit 0
`, started, gate)
	slowBin := filepath.Join(t.TempDir(), "slow-cli")
	if err := os.WriteFile(slowBin, []byte(slowScript), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"AGY_BIN", "CODEX_BIN", "COPILOT_BIN", "CLAUDE_BIN", "OPENCODE_BIN"} {
		t.Setenv(env, "/nonexistent/missing")
	}
	t.Setenv("CLAUDE_BIN", slowBin)

	w := &syncFrameWriter{firstCh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		printAllStatuses(context.Background(), w, true)
	}()

	// CLI完了(gate作成)前に最初のフレームが出ること
	select {
	case <-w.firstCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first frame not emitted before slow CLI finished")
	}
	w.mu.Lock()
	first := w.frames[0]
	w.mu.Unlock()
	if !strings.HasPrefix(first, "\x1b[?7l") || !strings.Contains(first, "  quota fetching...") {
		t.Fatalf("unexpected first frame: %q", first)
	}

	// gate を開いて完了させる
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("printAllStatuses did not finish")
	}

	w.mu.Lock()
	frames := append([]string(nil), w.frames...)
	w.mu.Unlock()
	if len(frames) < 3 {
		t.Fatalf("expected multiple live frames, got %d", len(frames))
	}
	lastWrite := frames[len(frames)-1]
	if lastWrite != "\x1b[?7h" {
		t.Fatalf("expected \\x1b[?7h at end, got %q", lastWrite)
	}
	lastFrame := frames[len(frames)-2]
	if strings.Contains(lastFrame, "fetching...") {
		t.Fatalf("fetching... remained in final frame:\n%s", lastFrame)
	}
	if !strings.Contains(lastFrame, "claude      slow-1.0  logged in\n  5h 75% left (resets 18:00)") {
		t.Fatalf("missing sanitized result in final frame:\n%s", lastFrame)
	}
	if strings.Contains(lastFrame, "\x07") || strings.Contains(lastFrame, "\t") {
		t.Fatalf("control chars not stripped in final frame:\n%q", lastFrame)
	}

	// キャンセル時に速やかに戻り、cancelled と \x1b[?7h が出ること
	_ = os.Remove(gate)
	_ = os.Remove(started)
	ctx, cancel := context.WithCancel(context.Background())
	w2 := &syncFrameWriter{firstCh: make(chan struct{})}
	done2 := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done2)
		printAllStatuses(ctx, w2, true)
	}()
	<-w2.firstCh
	cancel()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled printAllStatuses hung")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel took too long: %v", time.Since(start))
	}
	w2.mu.Lock()
	cFrames := append([]string(nil), w2.frames...)
	w2.mu.Unlock()
	if cFrames[len(cFrames)-1] != "\x1b[?7h" {
		t.Fatalf("expected \\x1b[?7h on cancel, got %q", cFrames[len(cFrames)-1])
	}
	if strings.Contains(cFrames[len(cFrames)-2], "fetching...") || !strings.Contains(cFrames[len(cFrames)-2], "  quota cancelled") {
		t.Fatalf("expected cancelled in final frame on cancel:\n%s", cFrames[len(cFrames)-2])
	}
}
