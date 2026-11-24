package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

		meters, reason := quota(context.Background(), "antigravity", bin)
		got := formatQuotaMCP(meters, reason, time.Now())
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
		if meters, reason := quota(context.Background(), "antigravity", bin); len(meters) != 0 || reason != "not available" {
			t.Fatalf("expected not available, got %+v %q", meters, reason)
		}
	})

	t.Run("codex", func(t *testing.T) {
		t.Setenv("FAKE_KIND", "codex")
		child := filepath.Join(t.TempDir(), "codex-app-pid")
		t.Setenv("FAKE_CHILD", child)
		soon := time.Now().Add(3 * time.Hour).Unix()
		t.Setenv("FAKE_CODEX_QUOTA", fmt.Sprintf(`{"id":2,"result":{"email":"secret@example.com","accountId":"acc-2","rateLimits":{"primary":{"usedPercent":14,"windowDurationMins":300,"resetsAt":%d},"secondary":null}}}`, soon))

		meters, reason := quota(context.Background(), "codex", bin)
		got := formatQuotaMCP(meters, reason, time.Now())
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
		if meters, reason := quota(context.Background(), "codex", bin); len(meters) != 0 || reason != "not available" {
			t.Fatalf("expected not available for null rateLimits, got %+v %q", meters, reason)
		}
	})

	t.Run("claude", func(t *testing.T) {
		t.Setenv("FAKE_KIND", "claude")
		arrPayload := `[{"type":"system","email":"secret@example.com"},{"type":"result","accountId":"acc-3","org":"org-3","result":"Header\nCurrent session: 22% used · resets Nov 24 at 5:50pm (Asia/Tokyo)\nCurrent week (all models): 15% used · resets Dec 1 at 11:32am (Asia/Tokyo)\nuser@example.com"}]`
		t.Setenv("FAKE_CLAUDE_QUOTA", arrPayload)
		meters, reason := quota(context.Background(), "claude", bin)
		got := formatQuotaMCP(meters, reason, time.Now())
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
		meters, reason = quota(context.Background(), "claude", bin)
		got = formatQuotaMCP(meters, reason, time.Now())
		if got != "5h 0% left" {
			t.Fatalf("expected clamped 0%%, got %q", got)
		}
	})

	t.Run("copilot_and_opencode_no_cli", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "argv")
		t.Setenv("FAKE_LOG", logPath)
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		for _, backend := range []string{"copilot", "opencode"} {
			if meters, reason := quota(context.Background(), backend, bin); len(meters) != 0 || reason != "not available" {
				t.Fatalf("%s: got %+v %q, want not available", backend, meters, reason)
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
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("LC_ALL", "C")

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
	for _, name := range []string{"agy", "codex", "claude", "opencode", "copilot"} {
		idx := strings.Index(s, " "+name)
		if idx <= pos {
			t.Fatalf("order mismatch for %s in:\n%s", name, s)
		}
		pos = idx
	}
	if !strings.Contains(s, "copilot   ✕ not-installed") {
		t.Fatalf("expected copilot not-installed in:\n%s", s)
	}
	if !strings.Contains(s, "quota unavailable") {
		t.Fatalf("expected quota unavailable in:\n%s", s)
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
	t.Setenv("LC_ALL", "C")
	t.Setenv("NO_COLOR", "1")
	oldSize := getTerminalSize
	getTerminalSize = func(io.Writer) (int, int) { return 40, 80 }
	t.Cleanup(func() { getTerminalSize = oldSize })

	ctx0, cancel0 := context.WithCancel(context.Background())
	defer cancel0()
	w := &syncFrameWriter{firstCh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		printAllStatuses(ctx0, w, true)
	}()
	t.Cleanup(func() {
		cancel0()
		<-done
	})

	// CLI完了(gate作成)前に最初のフレームが出ること
	select {
	case <-w.firstCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first frame not emitted before slow CLI finished")
	}
	w.mu.Lock()
	first := strings.Join(w.frames, "")
	w.mu.Unlock()
	if !strings.HasPrefix(first, "\x1b[?7l") || !strings.Contains(first, "fetching…") {
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
	if strings.Contains(lastFrame, "fetching…") {
		t.Fatalf("fetching… remained in final frame:\n%s", lastFrame)
	}
	if !strings.Contains(lastFrame, "claude    ● 1.0") || !strings.Contains(lastFrame, "75%") || !strings.Contains(lastFrame, "18:00") {
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
	if strings.Contains(cFrames[len(cFrames)-2], "fetching…") || !strings.Contains(cFrames[len(cFrames)-2], "cancelled") {
		t.Fatalf("expected cancelled in final frame on cancel:\n%s", cFrames[len(cFrames)-2])
	}
}

func TestReviewFixes(t *testing.T) {
	// 1. quotaClaude が --settings {"disableAllHooks":true} --setting-sources "" を渡すこと
	bin := fakeBinary(t)
	logPath := filepath.Join(t.TempDir(), "claude-argv")
	t.Setenv("FAKE_LOG", logPath)
	t.Setenv("FAKE_KIND", "claude")
	t.Setenv("FAKE_CLAUDE_QUOTA", `{"type":"result","result":"Current session: 10% used"}`)
	if got, reason := quota(context.Background(), "claude", bin); reason != "" || formatQuotaMCP(got, reason, time.Now()) != "5h 90% left" {
		t.Fatalf("claude quota: %+v %q", got, reason)
	}
	argvBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(argvBytes)
	if !strings.Contains(argv, "--settings\n{\"disableAllHooks\":true}\n--setting-sources\n\n") {
		t.Fatalf("claude quota missing hook-disable flags:\n%s", argv)
	}

	// 3. codex login status の生出力(マスク済みAPIキー等)を出さず exit 0/非0 のみで判定すること
	codexScript := `#!/bin/sh
if [ "$1" = "--version" ]; then printf 'codex 1.0\n'; exit 0; fi
if [ "$1" = "login" ]; then
	printf 'Logged in using API key sk-proj-MASKEDSECRET\n'
	exit "${FAKE_CODEX_EXIT:-0}"
fi
exit 0
`
	codexBin := filepath.Join(t.TempDir(), "codex-auth-cli")
	if err := os.WriteFile(codexBin, []byte(codexScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODEX_EXIT", "0")
	stOut, err := runStatus(context.Background(), "codex", codexBin)
	if err != nil || !strings.Contains(stOut, "auth: logged in") || strings.Contains(stOut, "MASKEDSECRET") {
		t.Fatalf("codex runStatus exit 0: %q, %v", stOut, err)
	}
	if auth := summarizeAuth(context.Background(), "codex", codexBin); auth != "logged in" {
		t.Fatalf("codex summarizeAuth exit 0: %q", auth)
	}
	t.Setenv("FAKE_CODEX_EXIT", "1")
	stOut, err = runStatus(context.Background(), "codex", codexBin)
	if err != nil || !strings.Contains(stOut, "auth: not logged in") || strings.Contains(stOut, "MASKEDSECRET") {
		t.Fatalf("codex runStatus exit 1: %q, %v", stOut, err)
	}
	if auth := summarizeAuth(context.Background(), "codex", codexBin); auth != "not logged in" {
		t.Fatalf("codex summarizeAuth exit 1: %q", auth)
	}

	// 4. codex app-server の 1 MiB 超行で "not available" になりプロセスが残らないこと
	bigPID := filepath.Join(t.TempDir(), "big-codex-pid")
	bigScript := fmt.Sprintf(`#!/bin/sh
echo $$ > %q
head -c 1048577 /dev/zero | tr '\0' 'a'
printf '\n'
sleep 30
`, bigPID)
	bigBin := filepath.Join(t.TempDir(), "codex-big-cli")
	if err := os.WriteFile(bigBin, []byte(bigScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, reason := quota(context.Background(), "codex", bigBin); len(got) != 0 || reason != "not available" {
		t.Fatalf("expected not available on >1MiB line, got %+v %q", got, reason)
	}
	data, err := os.ReadFile(bigPID)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		state, _ := exec.Command("ps", "-o", "state=", "-p", fmt.Sprint(pid)).Output()
		if st := strings.TrimSpace(string(state)); st != "" && !strings.HasPrefix(st, "Z") {
			t.Fatalf("big codex app-server still alive: %d (%s)", pid, state)
		}
	}

	// 6. /dev/null は isCharDevice で false になり MCP サーバー扱いになること
	dn, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dn.Close() }()
	if isCharDevice(dn) {
		t.Fatal("/dev/null must not be treated as a terminal")
	}
	var out, errOut bytes.Buffer
	if handled, code := runCLIArgs([]string{"aopm"}, isCharDevice(dn), false, &out, &errOut); handled || code != 0 {
		t.Fatalf("/dev/null should not trigger CLI status: handled=%v code=%d", handled, code)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStatusGoldenJAAndEN(t *testing.T) {
	now := time.Date(2026, 11, 24, 16, 47, 0, 0, time.FixedZone("JST", 9*3600))
	statuses := []backendStatus{
		{
			Name:        "antigravity",
			Installed:   true,
			VersionDone: true,
			Version:     "1.5.16",
			AuthDone:    true,
			Auth:        "logged in",
			QuotaDone:   true,
			Meters: []quotaMeter{
				{Group: "Gemini Models", Window: "5h", Left: 76, Reset: now.Add(32*time.Minute + 10*time.Second)},
				{Group: "Gemini Models", Window: "weekly", Left: 94, Reset: now.Add((6*24 + 19) * time.Hour)},
				{Group: "Claude and GPT models", Window: "5h", Left: 100},
			},
		},
		{
			Name:        "codex",
			Installed:   true,
			VersionDone: true,
			Version:     "0.163.0",
			AuthDone:    true,
			Auth:        "logged in",
			QuotaDone:   true,
			Meters: []quotaMeter{
				{Window: "5h", Left: 81, Reset: now.Add(18 * time.Minute)},
			},
		},
		{
			Name:        "claude",
			Installed:   true,
			VersionDone: true,
			Version:     "2.1.0",
			AuthDone:    true,
			Auth:        "logged in",
			QuotaDone:   true,
			Meters: []quotaMeter{
				{Window: "5h", Left: 40, Reset: now.Add(2*time.Hour + 5*time.Minute)},
			},
		},
		{
			Name:        "opencode",
			Installed:   true,
			VersionDone: true,
			Version:     "1.18.31",
			AuthDone:    true,
			Auth:        "logged in",
			QuotaDone:   true,
			Meters: []quotaMeter{
				{Window: "5h", Left: 70, Reset: now.Add(4 * time.Hour)},
				{Window: "weekly", Left: 50, Reset: now.Add(48 * time.Hour)},
				{Window: "monthly", Left: 15, Reset: now.Add(10 * 24 * time.Hour)},
			},
		},
		{
			Name:        "copilot",
			Installed:   true,
			VersionDone: true,
			Version:     "1.0.114",
			AuthDone:    true,
			Auth:        "not verified",
			QuotaDone:   true,
			QuotaReason: "not available",
		},
	}

	wantJA := strings.Join([]string{
		" agy       ● 1.5.16",
		"   Gemini    5h  ███████████████░░░░░   76%   あと 32分",
		"             週  ███████████████████░   94%   あと 6日19時間",
		"   他社      5h  ████████████████████  100%",
		"",
		" codex     ● 0.163.0",
		"             5h  ████████████████░░░░   81%   あと 18分",
		"",
		" claude    ● 2.1.0",
		"             5h  ████████░░░░░░░░░░░░   40%   あと 2時間5分",
		"",
		" opencode  ● 1.18.31",
		"             5h  ██████████████░░░░░░   70%   あと 4時間",
		"             週  ██████████░░░░░░░░░░   50%   あと 2日",
		"             月  ███░░░░░░░░░░░░░░░░░   15%   あと 10日",
		"",
		" copilot   ○ 1.0.114                    残量は取得できません",
	}, "\n") + "\n"

	gotJA := renderStatuses(statuses, statusRenderOpts{Now: now, JA: true, Color: false})
	if gotJA != wantJA {
		t.Fatalf("JA mismatch:\ngot:\n%s\nwant:\n%s", gotJA, wantJA)
	}

	wantEN := strings.Join([]string{
		" agy       ● 1.5.16",
		"   Gemini    5h     ███████████████░░░░░   76%   in 32m",
		"             week   ███████████████████░   94%   in 6d 19h",
		"   Other     5h     ████████████████████  100%",
		"",
		" codex     ● 0.163.0",
		"             5h     ████████████████░░░░   81%   in 18m",
		"",
		" claude    ● 2.1.0",
		"             5h     ████████░░░░░░░░░░░░   40%   in 2h 5m",
		"",
		" opencode  ● 1.18.31",
		"             5h     ██████████████░░░░░░   70%   in 4h",
		"             week   ██████████░░░░░░░░░░   50%   in 2d",
		"             month  ███░░░░░░░░░░░░░░░░░   15%   in 10d",
		"",
		" copilot   ○ 1.0.114                    quota unavailable",
	}, "\n") + "\n"

	gotEN := renderStatuses(statuses, statusRenderOpts{Now: now, JA: false, Color: false})
	if gotEN != wantEN {
		t.Fatalf("EN mismatch:\ngot:\n%s\nwant:\n%s", gotEN, wantEN)
	}

	// 幅80を超えないこと（長い日本語グループ名・長いResetText含む）
	longStatuses := append([]backendStatus(nil), statuses...)
	longStatuses[0].Meters = []quotaMeter{
		{Group: "とても長い日本語グループ名称テスト", Window: "monthly", Left: 55, ResetText: strings.Repeat("あ", 40)},
	}
	for _, out := range []string{gotJA, gotEN, renderStatuses(longStatuses, statusRenderOpts{Now: now, JA: true, Color: false})} {
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if w := displayWidth(line); w > statusMaxWidth {
				t.Fatalf("line exceeds width %d (%d): %q", statusMaxWidth, w, line)
			}
		}
	}
}

func TestStatusColorsAndBarAndRemaining(t *testing.T) {
	now := time.Date(2026, 11, 24, 16, 47, 0, 0, time.UTC)

	// 色の有無
	statuses := []backendStatus{
		{
			Name: "codex", Installed: true, VersionDone: true, Version: "1.0.0",
			AuthDone: true, Auth: "logged in", QuotaDone: true,
			Meters: []quotaMeter{
				{Window: "5h", Left: 50},
				{Window: "weekly", Left: 49},
				{Window: "monthly", Left: 19},
			},
		},
		{
			Name: "copilot", Installed: true, VersionDone: true, Version: "1.0.0",
			AuthDone: true, Auth: "not verified", QuotaDone: true, QuotaReason: "not available",
		},
		{
			Name: "claude", Installed: false, VersionDone: true, AuthDone: true, QuotaDone: true,
		},
	}
	colored := renderStatuses(statuses, statusRenderOpts{Now: now, JA: false, Color: true})
	for _, code := range []string{"\x1b[32m●\x1b[0m", "\x1b[90m○\x1b[0m", "\x1b[31m✕\x1b[0m", "\x1b[32m", "\x1b[33m", "\x1b[31m", "\x1b[90m░"} {
		if !strings.Contains(colored, code) {
			t.Fatalf("missing color code %q in:\n%q", code, colored)
		}
	}
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(true) {
		t.Fatal("NO_COLOR=1 must disable color")
	}
	t.Setenv("NO_COLOR", "")
	if colorEnabled(false) {
		t.Fatal("non-live pipe must disable color")
	}

	// バー端数 (0, 1, 99, 100)
	barCases := map[int]string{
		0:   "░░░░░░░░░░░░░░░░░░░░",
		1:   "█░░░░░░░░░░░░░░░░░░░",
		99:  "███████████████████░",
		100: "████████████████████",
	}
	for pct, want := range barCases {
		if got := renderBar(pct, 20, false); got != want {
			t.Fatalf("bar(%d) = %q, want %q", pct, got, want)
		}
	}

	// 残り時間境界
	remCases := []struct {
		reset time.Time
		text  string
		ja    bool
		want  string
	}{
		{now.Add(-time.Minute), "", true, "まもなく"},
		{now.Add(-time.Minute), "", false, "soon"},
		{now, "", true, "まもなく"},
		{now.Add(59 * time.Minute), "", true, "あと 59分"},
		{now.Add(60 * time.Minute), "", true, "あと 1時間"},
		{now.Add(60 * time.Minute), "", false, "in 1h"},
		{now.Add(95 * time.Minute), "", true, "あと 1時間35分"},
		{now.Add(24 * time.Hour), "", true, "あと 1日"},
		{now.Add(24 * time.Hour), "", false, "in 1d"},
		{now.Add(25 * time.Hour), "", false, "in 1d 1h"},
		{time.Time{}, "18:00", true, "18:00"},
	}
	for _, tc := range remCases {
		if got := formatRemaining(tc.reset, tc.text, now, tc.ja); got != tc.want {
			t.Fatalf("formatRemaining(%v, %q, ja=%v) = %q, want %q", tc.reset, tc.text, tc.ja, got, tc.want)
		}
	}
}

func TestClaudeResetParsing(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	now := time.Date(2026, 11, 24, 16, 47, 0, 0, jst)

	t1, txt1 := parseClaudeReset("Nov 24 at 5:50pm (Asia/Tokyo)", now)
	if txt1 != "" || t1.Year() != 2026 || t1.Month() != time.November || t1.Day() != 24 || t1.Hour() != 17 || t1.Minute() != 50 {
		t.Fatalf("t1=%v txt=%q", t1, txt1)
	}
	t2, txt2 := parseClaudeReset("Nov 25 at 3am (Asia/Tokyo)", now)
	if txt2 != "" || t2.Year() != 2026 || t2.Month() != time.November || t2.Day() != 25 || t2.Hour() != 3 || t2.Minute() != 0 {
		t.Fatalf("t2=%v txt=%q", t2, txt2)
	}
	// 半年以上過去なら翌年
	decNow := time.Date(2026, 12, 28, 12, 0, 0, 0, jst)
	t3, txt3 := parseClaudeReset("Jan 2 at 3pm (Asia/Tokyo)", decNow)
	if txt3 != "" || t3.Year() != 2027 || t3.Month() != time.January || t3.Day() != 2 || t3.Hour() != 15 {
		t.Fatalf("year rollover: t3=%v txt=%q", t3, txt3)
	}
	// 解析失敗は ResetText
	t4, txt4 := parseClaudeReset("18:00", now)
	if !t4.IsZero() || txt4 != "18:00" {
		t.Fatalf("fallback: t4=%v txt=%q", t4, txt4)
	}
}

func TestOpenCodeGoQuotaAndSecurity(t *testing.T) {
	oldTransport := opencodeTransport
	oldURL := opencodeUsageURL
	t.Cleanup(func() {
		opencodeTransport = oldTransport
		opencodeUsageURL = oldURL
	})

	writeAuth := func(t *testing.T, content string) string {
		t.Helper()
		xdg := t.TempDir()
		p := filepath.Join(xdg, "opencode", "auth.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_DATA_HOME", xdg)
		return xdg
	}

	const secretGoKey = "sk-go-secret-token-123"
	const otherKey = "sk-other-provider-999"

	t.Run("normal_and_missing_items", func(t *testing.T) {
		writeAuth(t, fmt.Sprintf(`{"anthropic":{"type":"api","key":%q},"opencode-go":{"type":"api","key":%q}}`, otherKey, secretGoKey))
		opencodeUsageURL = "https://opencode.ai/inference/go/v1/usage"
		var gotAuth string
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			body := `{"usage":{"rolling":{"status":"ok","percent":30,"resetsAt":"2026-11-24T17:19:00+09:00"},"monthly":{"status":"ok","percent":10,"resetsAt":"2026-12-01T00:00:00Z"}}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})
		meters, reason := quota(context.Background(), "opencode", "/nonexistent/opencode")
		if reason != "" || len(meters) != 2 {
			t.Fatalf("got meters=%+v reason=%q", meters, reason)
		}
		if gotAuth != "Bearer "+secretGoKey {
			t.Fatalf("unexpected auth header: %q", gotAuth)
		}
		if meters[0].Window != "5h" || meters[0].Left != 70 || meters[1].Window != "monthly" || meters[1].Left != 90 {
			t.Fatalf("unexpected meters: %+v", meters)
		}

		// runStatus で logged in になり、秘密鍵を含まないこと
		bin := fakeBinary(t)
		st, err := runStatus(context.Background(), "opencode", bin)
		if err != nil || !strings.Contains(st, "auth: logged in") || strings.Contains(st, secretGoKey) || strings.Contains(st, otherKey) {
			t.Fatalf("runStatus: %q, %v", st, err)
		}
	})

	t.Run("401_and_redirect_and_huge_body", func(t *testing.T) {
		writeAuth(t, fmt.Sprintf(`{"opencode-go":{"type":"api","key":%q}}`, secretGoKey))
		opencodeUsageURL = "https://opencode.ai/inference/go/v1/usage"

		// 401
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("unauthorized " + secretGoKey)), Header: make(http.Header)}, nil
		})
		if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" {
			t.Fatalf("401: %+v %q", m, r)
		}

		// 302 リダイレクトで転送先へリクエストが飛ばないこと
		redirected := false
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "opencode.ai" {
				redirected = true
			}
			h := make(http.Header)
			h.Set("Location", "https://evil.example/leak")
			return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(strings.NewReader("")), Header: h}, nil
		})
		if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" {
			t.Fatalf("302: %+v %q", m, r)
		}
		if redirected {
			t.Fatal("redirect target was called")
		}

		// 64KiB 超応答
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			big := `{"usage":{"rolling":{"percent":10}}}` + strings.Repeat(" ", opencodeResponseLimit+100)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(big)), Header: make(http.Header)}, nil
		})
		if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" {
			t.Fatalf("huge: %+v %q", m, r)
		}
	})

	t.Run("no_http_without_valid_key", func(t *testing.T) {
		called := false
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("unexpected call with %s", secretGoKey)
		})
		// ファイル無し
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" || called {
			t.Fatalf("missing file: %+v %q called=%v", m, r, called)
		}
		// 空白・制御文字・type不一致
		for _, bad := range []string{
			`{"opencode-go":{"type":"oauth","key":"abc"}}`,
			`{"opencode-go":{"type":"api","key":"abc def"}}`,
			`{"opencode-go":{"type":"api","key":"abc\ndef"}}`,
			`{"anthropic":{"type":"api","key":"abc"}}`,
		} {
			writeAuth(t, bad)
			if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" || called {
				t.Fatalf("bad auth %s: %+v %q called=%v", bad, m, r, called)
			}
		}
	})
}

func TestLowTerminalNoCursorUp(t *testing.T) {
	for _, env := range []string{"AGY_BIN", "CODEX_BIN", "COPILOT_BIN", "CLAUDE_BIN", "OPENCODE_BIN"} {
		t.Setenv(env, "/nonexistent/missing")
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("NO_COLOR", "1")
	oldSize := getTerminalSize
	getTerminalSize = func(io.Writer) (int, int) { return 5, 80 }
	t.Cleanup(func() { getTerminalSize = oldSize })

	var buf bytes.Buffer
	printAllStatuses(context.Background(), &buf, true)
	out := buf.String()
	cursorUpRE := regexp.MustCompile(`\x1b\[\d+A`)
	if cursorUpRE.MatchString(out) {
		t.Fatalf("cursor-up escape emitted on low terminal:\n%q", out)
	}
	if !strings.Contains(out, "\r\x1b[K") {
		t.Fatalf("expected \\r\\x1b[K clear sequence:\n%q", out)
	}
	if !strings.Contains(out, "copilot   ✕ not-installed") {
		t.Fatalf("expected final status output:\n%q", out)
	}
}

func TestQuotaMeterOrderWithinGroup(t *testing.T) {
	bin := fakeBinary(t)
	t.Setenv("FAKE_KIND", "antigravity")
	t.Setenv("AGY_BIN", bin)
	t.Setenv("FAKE_AGY_QUOTA", `{"command":{"data":{"groups":[{"name":"G2","buckets":[{"window":"otherB","remaining_fraction":0.1},{"window":"monthly","remaining_fraction":0.2},{"window":"otherA","remaining_fraction":0.3},{"window":"weekly","remaining_fraction":0.4},{"window":"5h","remaining_fraction":0.5}]},{"name":"G1","buckets":[{"window":"weekly","remaining_fraction":0.6},{"window":"5h","remaining_fraction":0.7}]}]}}}`)

	meters, reason := quota(context.Background(), "antigravity", bin)
	if reason != "" {
		t.Fatalf("unexpected reason: %q", reason)
	}
	want := []string{"G2:5h", "G2:weekly", "G2:monthly", "G2:otherB", "G2:otherA", "G1:5h", "G1:weekly"}
	if len(meters) != len(want) {
		t.Fatalf("len=%d want %d: %+v", len(meters), len(want), meters)
	}
	for i, m := range meters {
		if got := m.Group + ":" + m.Window; got != want[i] {
			t.Fatalf("idx %d = %q, want %q", i, got, want[i])
		}
	}
}

type cancelBodyReader struct {
	cancel func()
}

func (r cancelBodyReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, context.Canceled
}

func (r cancelBodyReader) Close() error { return nil }

func TestReviewFixItems(t *testing.T) {
	// 1 & 2 & 3: auth.json (FIFO/symlink/1MiB), OPENCODE_AUTH_CONTENT, body read cancel
	t.Run("opencode_auth_safety_and_body_cancel", func(t *testing.T) {
		oldTransport := opencodeTransport
		t.Cleanup(func() { opencodeTransport = oldTransport })

		called := false
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"usage":{"rolling":{"percent":10}}}`)), Header: make(http.Header)}, nil
		})

		// FIFO で固まらないこと
		xdg := t.TempDir()
		authDir := filepath.Join(xdg, "opencode")
		if err := os.MkdirAll(authDir, 0o700); err != nil {
			t.Fatal(err)
		}
		fifoPath := filepath.Join(authDir, "auth.json")
		if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_DATA_HOME", xdg)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" {
				t.Errorf("fifo: %+v %q", m, r)
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("FIFO read blocked")
		}
		if called {
			t.Fatal("HTTP called on FIFO")
		}

		// symlink 先が通常ファイルなら可
		realFile := filepath.Join(t.TempDir(), "real-auth.json")
		if err := os.WriteFile(realFile, []byte(`{"opencode-go":{"type":"api","key":"sk-symlink-ok"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(fifoPath)
		if err := os.Symlink(realFile, fifoPath); err != nil {
			t.Fatal(err)
		}
		if m, r := quota(context.Background(), "opencode", ""); r != "" || len(m) != 1 || !called {
			t.Fatalf("symlink regular file failed: %+v %q called=%v", m, r, called)
		}

		// 1MiB 超は拒否
		called = false
		bigAuth := `{"opencode-go":{"type":"api","key":"sk-big"}}` + strings.Repeat(" ", 1<<20)
		if err := os.WriteFile(realFile, []byte(bigAuth), 0o600); err != nil {
			t.Fatal(err)
		}
		if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" || called {
			t.Fatalf(">1MiB auth accepted: %+v %q called=%v", m, r, called)
		}

		// OPENCODE_AUTH_CONTENT が空でなければ通信しない
		if err := os.WriteFile(realFile, []byte(`{"opencode-go":{"type":"api","key":"sk-ok"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OPENCODE_AUTH_CONTENT", "{}")
		if m, r := quota(context.Background(), "opencode", ""); len(m) != 0 || r != "not available" || called {
			t.Fatalf("OPENCODE_AUTH_CONTENT did not block: %+v %q called=%v", m, r, called)
		}
		t.Setenv("OPENCODE_AUTH_CONTENT", "")

		// 本文読み取り中の cancel -> cancelled
		ctx, cancel := context.WithCancel(context.Background())
		opencodeTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: cancelBodyReader{cancel: cancel}, Header: make(http.Header)}, nil
		})
		if m, r := quota(ctx, "opencode", ""); len(m) != 0 || r != "cancelled" {
			t.Fatalf("body cancel: %+v %q", m, r)
		}
	})

	// 4: 狭い端末幅 (20, 32, 40, 60, 80)
	t.Run("narrow_terminal_widths", func(t *testing.T) {
		now := time.Date(2026, 11, 24, 16, 47, 0, 0, time.UTC)
		statuses := []backendStatus{
			{
				Name: "antigravity", Installed: true, VersionDone: true, Version: "1.5.16",
				AuthDone: true, Auth: "logged in", QuotaDone: true,
				Meters: []quotaMeter{{Group: "Gemini Models", Window: "5h", Left: 76, Reset: now.Add(32 * time.Minute)}},
			},
			{
				Name: "copilot", Installed: true, VersionDone: true, Version: "1.0.114",
				AuthDone: true, Auth: "not verified", QuotaDone: true, QuotaReason: "not available",
			},
		}
		for _, w := range []int{20, 32, 40, 60, 80} {
			for _, ja := range []bool{false, true} {
				out := renderStatuses(statuses, statusRenderOpts{Now: now, JA: ja, Color: false, Width: w})
				for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
					if dw := displayWidth(line); dw > w {
						t.Fatalf("width=%d ja=%v exceeded (%d): %q", w, ja, dw, line)
					}
				}
			}
		}
		// 幅40ではバーが縮むこと、幅32ではバーが省かれて%とリセットが残ること
		out40 := renderStatuses(statuses, statusRenderOpts{Now: now, JA: true, Width: 40})
		if !strings.Contains(out40, "█") || strings.Contains(out40, strings.Repeat("█", 15)) {
			t.Fatalf("width 40 should shrink bar:\n%s", out40)
		}
		out32 := renderStatuses(statuses, statusRenderOpts{Now: now, JA: true, Width: 32})
		if strings.Contains(out32, "█") || !strings.Contains(out32, "76%") || !strings.Contains(out32, "あと 32分") {
			t.Fatalf("width 32 should omit bar and keep %% + reset:\n%s", out32)
		}
	})

	// 5: 端末縮小で前フレームが現在の高さに入らない時はカーソルを戻さない
	t.Run("terminal_shrink_no_cursor_up", func(t *testing.T) {
		for _, env := range []string{"AGY_BIN", "CODEX_BIN", "COPILOT_BIN", "CLAUDE_BIN", "OPENCODE_BIN"} {
			t.Setenv(env, "/nonexistent/missing")
		}
		t.Setenv("LC_ALL", "C")
		t.Setenv("NO_COLOR", "1")
		calls := 0
		oldSize := getTerminalSize
		getTerminalSize = func(io.Writer) (int, int) {
			calls++
			if calls == 1 {
				return 40, 80
			}
			return 4, 80
		}
		t.Cleanup(func() { getTerminalSize = oldSize })

		var buf bytes.Buffer
		printAllStatuses(context.Background(), &buf, true)
		if regexp.MustCompile(`\x1b\[\d+A`).MatchString(buf.String()) {
			t.Fatalf("cursor-up emitted after shrink:\n%q", buf.String())
		}
	})

	// 6: 認証未完了時は残量が先に出ても「…」を表示
	t.Run("auth_pending_icon", func(t *testing.T) {
		st := []backendStatus{{
			Name: "claude", Installed: true, VersionDone: true, Version: "2.1.0",
			AuthDone: false, Auth: "", QuotaDone: true,
			Meters: []quotaMeter{{Window: "5h", Left: 80}},
		}}
		out := renderStatuses(st, statusRenderOpts{Now: time.Now(), Width: 80})
		if !strings.Contains(out, "claude    … 2.1.0") {
			t.Fatalf("expected pending icon …, got:\n%s", out)
		}
	})

	// 7: 表示幅（半角カナ=1, 結合文字=0）
	t.Run("display_width_kana_and_combining", func(t *testing.T) {
		if got := displayWidth("ｱｲｳ"); got != 3 {
			t.Fatalf("halfwidth kana width = %d, want 3", got)
		}
		if got := displayWidth("｡｢｣"); got != 3 {
			t.Fatalf("halfwidth punct width = %d, want 3", got)
		}
		if got := displayWidth("e\u0301"); got != 1 {
			t.Fatalf("combining acute width = %d, want 1", got)
		}
		if got := displayWidth("か\u3099"); got != 2 {
			t.Fatalf("combining dakuten width = %d, want 2", got)
		}
		if got := displayWidth("a\u20dd"); got != 1 {
			t.Fatalf("enclosing mark width = %d, want 1", got)
		}
	})

	// 8: バージョンプレリリース接尾辞と末尾ドット除去
	t.Run("extract_version_prerelease", func(t *testing.T) {
		cases := map[string]string{
			"GitHub Copilot CLI 1.0.114.": "1.0.114",
			"codex 0.163.0-rc.1.":         "0.163.0-rc.1",
			"agy 1.5.16-alpha.2":          "1.5.16-alpha.2",
		}
		for in, want := range cases {
			if got := extractVersion(in); got != want {
				t.Fatalf("extractVersion(%q) = %q, want %q", in, got, want)
			}
		}
	})
}
