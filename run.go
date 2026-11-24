package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stdoutLimit = 32 << 20
	stderrLimit = 64 << 10
	// mcpm が親を SIGKILL する前に、別プロセスグループの子を片付け終えるための猶予。
	killGrace = time.Second
)

type processResult struct {
	Stdout, Stderr string
	ExitCode       int
	StartErr       error
	WaitErr        error
	TimedOut       bool
	Cancelled      bool
	StdoutTooLarge bool
}

var sessionMu sync.Mutex

func sessionPath(backend, workspace string) string {
	root := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(root) {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".local", "state")
	}
	h := sha256.Sum256([]byte(workspace))
	return filepath.Join(root, "aopm", backend, hex.EncodeToString(h[:])+".json")
}

func loadSession(backend, workspace string) (sessionPin, bool, error) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	b, err := os.ReadFile(sessionPath(backend, workspace))
	if os.IsNotExist(err) {
		return sessionPin{}, false, nil
	}
	if err != nil {
		return sessionPin{}, false, fmt.Errorf("cannot read saved session: %w", err)
	}
	var p sessionPin
	if json.Unmarshal(b, &p) != nil || strings.TrimSpace(p.Session) == "" || p.Sandbox == "" {
		return sessionPin{}, false, fmt.Errorf("saved session is corrupt")
	}
	if backend == "antigravity" && p.Sandbox == "none" { // sandbox対応前に保存された記録
		p.Sandbox = "read-only"
	}
	if p.Sandbox != "read-only" && p.Sandbox != "workspace-write" {
		return sessionPin{}, false, fmt.Errorf("saved session has an invalid sandbox")
	}
	return p, true, nil
}

func saveSession(backend, workspace string, p sessionPin) error {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	path := sessionPath(backend, workspace)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(dir), 0o700)
	_ = os.Chmod(dir, 0o700)
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-*")
	if err != nil {
		return err
	}
	name, ok := tmp.Name(), false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(b, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Close()
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return err
	}
	ok = true
	return nil
}

func runCLI(parent context.Context, args []string, dir string, stdin []byte, env []string, timeout time.Duration) processResult {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	r := processResult{ExitCode: -1}
	if err := ctx.Err(); err != nil {
		r.Cancelled = err == context.Canceled
		r.TimedOut = err == context.DeadlineExceeded
		return r
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if env != nil {
		cmd.Env = env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 700 * time.Millisecond
	stdout, stderr := &captureWriter{limit: stdoutLimit}, &captureWriter{limit: stderrLimit, tail: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		r.StartErr = err
		return r
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		r.TimedOut = ctx.Err() == context.DeadlineExceeded
		r.Cancelled = !r.TimedOut
		signalGroup(pid, syscall.SIGTERM)
		timer := time.NewTimer(killGrace)
		select {
		case waitErr = <-waitCh:
			timer.Stop()
		case <-timer.C:
			signalGroup(pid, syscall.SIGKILL)
			waitErr = <-waitCh
		}
	}
	if pid > 0 {
		signalGroup(pid, syscall.SIGKILL)
	}
	r.WaitErr = waitErr
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	r.Stdout, r.Stderr, r.StdoutTooLarge = string(stdout.data), string(stderr.data), stdout.tooLarge
	return r
}

type captureWriter struct {
	limit    int
	tail     bool
	data     []byte
	tooLarge bool
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.tail {
		w.data = append(w.data, p...)
		if len(w.data) > w.limit {
			w.data = append([]byte(nil), w.data[len(w.data)-w.limit:]...)
		}
		return len(p), nil
	}
	if len(w.data) < w.limit {
		n := len(p)
		if len(w.data)+n > w.limit {
			n = w.limit - len(w.data)
			w.tooLarge = true
		}
		w.data = append(w.data, p[:n]...)
	} else {
		w.tooLarge = true
	}
	return len(p), nil
}

func signalGroup(pid int, sig syscall.Signal) {
	if pid > 0 {
		_ = syscall.Kill(-pid, sig)
	}
}

func commandFailure(name string, r processResult, timeout int, detail string) error {
	if r.StartErr != nil {
		return fmt.Errorf("%s: %w", name, r.StartErr)
	}
	if r.TimedOut {
		return fmt.Errorf("%s timed out after %ds. Raise timeout_s.", name, timeout)
	}
	if r.Cancelled {
		return context.Canceled
	}
	if r.StdoutTooLarge {
		return fmt.Errorf("%s produced more than 32 MiB on stdout", name)
	}
	if r.ExitCode != 0 {
		if detail == "" {
			detail = validTail(r.Stdout, 500)
		}
		msg := fmt.Sprintf("%s exited %d", name, r.ExitCode)
		if detail != "" {
			msg += ": " + detail
		}
		if tail := validTail(r.Stderr, 1000); tail != "" {
			msg += "\nstderr: " + tail
		}
		if r.WaitErr != nil && r.ExitCode < 0 {
			msg += "\n" + r.WaitErr.Error()
		}
		return errors.New(msg)
	}
	if detail != "" {
		return fmt.Errorf("%s: %s", name, detail)
	}
	return nil
}

func statusProcessError(name string, r processResult) error {
	if r.StartErr != nil {
		return fmt.Errorf("%s is not installed: %w", name, r.StartErr)
	}
	if r.TimedOut {
		return fmt.Errorf("%s --version timed out", name)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s --version exited %d: %s", name, r.ExitCode, validTail(r.Stderr, 1000))
	}
	return nil
}

func validTail(s string, n int) string {
	b := []byte(s)
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), "�"))
}

func runBackend(ctx context.Context, o runOptions) (backendResult, error) {
	switch o.Backend {
	case "antigravity":
		return runAntigravity(ctx, o)
	case "codex":
		return runCodex(ctx, o)
	case "copilot":
		return runCopilot(ctx, o)
	case "claude":
		return runClaude(ctx, o)
	case "opencode":
		return runOpenCode(ctx, o)
	default:
		return backendResult{}, fmt.Errorf("unknown backend %s", o.Backend)
	}
}

// antigravitySettings は呼び出し単位で利用者の設定を上書きする。headlessでは確認が必要な操作は
// 自動拒否される。agentModeを空にしないと既定のAutoモードになり、サンドボックスのバイパスが
// AIの審査だけで承認されてしまう。MCPサーバーは境界の外で動くので両モードで禁止する。
func antigravitySettings(sandbox string) string {
	deny := `"mcp(*)","unsandboxed(*)"`
	if sandbox != "workspace-write" {
		deny += `,"command(*)","write_file(*)"`
	}
	return `{"agentMode":"","toolPermission":"proceed-in-sandbox","allowNonWorkspaceAccess":false,"permissions":{"allow":["read_file(*)"],"deny":[` + deny + `]}}`
}

// 拒否されるとターンごと終わるため、バイパスを試させない。
const antigravityNote = "\n\n[Sandbox: only the workspace and the temp directory are writable and there is no network. Never set BypassSandbox and never write elsewhere; a denied action ends this run. If a tool cache is not writable, point it at the temp directory (e.g. GOCACHE=/tmp/go-cache).]"

func runAntigravity(ctx context.Context, o runOptions) (backendResult, error) {
	fields := strings.Fields(o.Prompt)
	first := ""
	if len(fields) != 0 {
		first = strings.TrimSpace(fields[0])
	}
	// 単独セグメントの "/x" だけがコマンド展開される。"/etc/hosts" 等のパスは通す。
	if o.Plan && strings.HasPrefix(first, "/") && !strings.Contains(first[1:], "/") {
		return backendResult{}, fmt.Errorf("plan mode refuses slash-command prompts")
	}
	args := []string{o.Binary, "--print-timeout", fmt.Sprintf("%ds", o.Timeout), "--sandbox", "--settings", antigravitySettings(o.Sandbox)}
	if o.Plan {
		args = append(args, "--mode", "plan")
	} else {
		args = append(args, "--disable-slash-commands")
		if o.Sandbox == "workspace-write" {
			args = append(args, "--mode", "accept-edits")
		}
	}
	args = append(args, "--output-format", "json")
	if len(o.Schema) != 0 {
		args = append(args, "--json-schema", string(o.Schema))
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.Continue {
		args = append(args, "--conversation", o.Session)
	}
	prompt := o.Prompt
	if o.Sandbox == "workspace-write" {
		prompt += antigravityNote
	}
	args = append(args, "-p", prompt)
	r := runCLI(ctx, args, o.Workspace, nil, nil, time.Duration(o.Timeout+30)*time.Second)
	obj, _ := findJSONObject(r.Stdout, "response")
	if r.TimedOut || strings.Contains(strings.ToLower(r.Stderr), "print timeout after") {
		partial := responseText(obj)
		return backendResult{}, antigravityTimeoutError(o.Timeout, partial)
	}
	if err := commandFailure("agy", r, o.Timeout, ""); err != nil {
		return backendResult{}, err
	}
	if obj != nil {
		if reason := rawString(obj["finished_reason"]); reason != "" {
			return backendResult{}, fmt.Errorf("agy ended the turn early (finished_reason=%s): %s", reason, responseText(obj))
		}
		if raw, exists := obj["status"]; exists {
			status := rawString(raw)
			if status != "SUCCESS" {
				return backendResult{}, fmt.Errorf("agy reported status=%s: %s", status, responseText(obj))
			}
		}
	}
	if obj == nil {
		return backendResult{}, fmt.Errorf("agy produced no JSON result containing response")
	}
	answer := responseText(obj)
	denied := string(obj["denied_actions"])
	if denied == "null" || denied == "[]" {
		denied = ""
	}
	if strings.TrimSpace(answer) == "" {
		if denied != "" {
			// agyは拒否された時点で終わる。それまでの編集は残るので、再開できるようセッションを返す。
			return backendResult{Session: rawString(obj["conversation_id"])}, fmt.Errorf("agy stopped: the sandbox=%q policy denied %s; any earlier edits remain, use antigravity_continue to resume without that action", o.Sandbox, denied)
		}
		return backendResult{}, fmt.Errorf("agy produced an empty response")
	}
	if len(o.Schema) != 0 {
		structured := obj["structured_output"]
		if len(structured) == 0 || string(structured) == "null" {
			return backendResult{}, fmt.Errorf("agy returned no structured_output for a schema run")
		}
		var b bytes.Buffer
		if json.Compact(&b, structured) != nil {
			return backendResult{}, fmt.Errorf("agy returned invalid structured_output")
		}
		answer = b.String()
	} else if denied != "" {
		answer += fmt.Sprintf("\nWarning: the sandbox=%q policy denied %s; the task may be incomplete.", o.Sandbox, denied)
	}
	return backendResult{Answer: answer, Session: rawString(obj["conversation_id"])}, nil
}

func antigravityTimeoutError(timeout int, partial string) error {
	msg := fmt.Sprintf("agy hit its %ds timeout before finishing, so it returned only part of an answer. Raise timeout_s.", timeout)
	if strings.TrimSpace(partial) != "" {
		msg += "\n\n--- partial answer ---\n" + partial
	}
	return errors.New(msg)
}

func findJSONObject(text, required string) (map[string]json.RawMessage, bool) {
	text = strings.ToValidUTF8(text, "�")
	for start := strings.IndexByte(text, '{'); start >= 0; {
		var obj map[string]json.RawMessage
		dec := json.NewDecoder(strings.NewReader(text[start:]))
		if dec.Decode(&obj) == nil && obj != nil {
			if _, ok := obj[required]; ok {
				return obj, true
			}
		}
		next := strings.IndexByte(text[start+1:], '{')
		if next < 0 {
			break
		}
		start += next + 1
	}
	return nil, false
}

func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) != 0 && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func responseText(obj map[string]json.RawMessage) string {
	return rawString(obj["response"])
}

func runCodex(ctx context.Context, o runOptions) (backendResult, error) {
	args := []string{o.Binary, "exec"}
	if o.Continue {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check")
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+o.Effort)
	}
	if o.Continue {
		args = append(args, "-c", "sandbox_mode="+o.Sandbox, "--", o.Session, "-")
	} else {
		args = append(args, "-s", o.Sandbox, "-C", o.Workspace, "--", "-")
	}
	r := runCLI(ctx, args, o.Workspace, []byte(o.Prompt), nil, time.Duration(o.Timeout)*time.Second)
	answer, session, detail, failed, completed := parseCodex(r.Stdout)
	if err := commandFailure("codex", r, o.Timeout, detailIfExit(r, detail)); err != nil {
		return backendResult{}, err
	}
	if failed || !completed {
		if detail == "" {
			detail = "no turn.completed event"
		}
		return backendResult{}, fmt.Errorf("codex failed: %s", detail)
	}
	if strings.TrimSpace(answer) == "" {
		return backendResult{}, fmt.Errorf("codex produced no agent message")
	}
	if strings.Contains(strings.ToLower(r.Stderr), "blocked by policy") {
		answer += "\n\nWarning: codex sandbox blocked one or more commands by policy; verify the answer against the workspace."
	}
	return backendResult{Answer: strings.TrimSpace(answer), Session: session}, nil
}

func parseCodex(text string) (answer, session, detail string, failed, completed bool) {
	for _, line := range strings.Split(strings.ToValidUTF8(text, "�"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch rawString(e["type"]) {
		case "thread.started":
			session = rawString(e["thread_id"])
		case "item.completed":
			var item map[string]json.RawMessage
			_ = json.Unmarshal(e["item"], &item)
			if rawString(item["type"]) == "agent_message" {
				answer = rawString(item["text"])
			}
		case "turn.failed":
			failed = true
			detail = nestedError(e["error"])
		case "turn.completed":
			completed = true
		case "error":
			if detail == "" {
				detail = rawString(e["message"])
			}
		}
	}
	return
}

func nestedError(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		if s := rawString(m["message"]); s != "" {
			return s
		}
	}
	return rawString(raw)
}

func runCopilot(ctx context.Context, o runOptions) (backendResult, error) {
	model := o.Model
	if model == "" {
		model = "auto"
	}
	args := []string{o.Binary, "--session-id", o.Session, "-C", o.Workspace, "--no-ask-user", "--no-color", "--no-auto-update", "--disable-builtin-mcps", "--allow-all-tools"}
	if o.Sandbox == "read-only" {
		args = append(args, "--deny-tool=write", "--deny-tool=shell", "--available-tools=view,grep,glob")
	}
	args = append(args, "--model", model)
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	args = append(args, "-s", "-p", o.Prompt)
	r := runCLI(ctx, args, o.Workspace, nil, nil, time.Duration(o.Timeout)*time.Second)
	if err := commandFailure("copilot", r, o.Timeout, ""); err != nil {
		return backendResult{}, err
	}
	answer := strings.TrimSpace(strings.ToValidUTF8(r.Stdout, "�"))
	if answer == "" {
		return backendResult{}, fmt.Errorf("copilot produced no output on stdout; stderr: %s", validTail(r.Stderr, 300))
	}
	return backendResult{Answer: answer, Session: o.Session}, nil
}

func runClaude(ctx context.Context, o runOptions) (backendResult, error) {
	mode := "dontAsk"
	args := []string{o.Binary, "-p", "--output-format", "json", "--disable-slash-commands", "--strict-mcp-config", "--restricted", "--permission-mode", mode, "--permission-prompts", "none"}
	if o.Sandbox == "workspace-write" {
		args[8] = "acceptEdits"
	}
	if o.Sandbox == "read-only" {
		args = append(args, "--disallowedTools=Edit,Write,NotebookEdit")
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.Continue {
		args = append(args, "--resume", o.Session)
	}
	r := runCLI(ctx, args, o.Workspace, []byte(o.Prompt), claudeEnv(), time.Duration(o.Timeout)*time.Second)
	obj, ok := findJSONObject(r.Stdout, "result")
	detail := ""
	if obj != nil {
		detail = rawString(obj["result"])
		if detail == "" {
			detail = rawString(obj["subtype"])
		}
	}
	if err := commandFailure("claude", r, o.Timeout, detailIfExit(r, detail)); err != nil {
		return backendResult{}, err
	}
	if !ok {
		return backendResult{}, fmt.Errorf("claude produced no JSON result")
	}
	var isError bool
	_ = json.Unmarshal(obj["is_error"], &isError)
	if isError {
		if detail == "" {
			detail = "agent reported an error"
		}
		return backendResult{}, fmt.Errorf("claude error: %s", detail)
	}
	answer := rawString(obj["result"])
	if strings.TrimSpace(answer) == "" {
		return backendResult{}, fmt.Errorf("claude produced an empty result")
	}
	return backendResult{Answer: strings.TrimSpace(answer), Session: rawString(obj["session_id"])}, nil
}

func detailIfExit(r processResult, detail string) string {
	if r.ExitCode != 0 {
		return detail
	}
	return ""
}

func claudeEnv() []string {
	drop := map[string]bool{"CLAUDECODE": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ATTENDED": true, "CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true, "CLAUDE_PID": true, "AI_AGENT": true, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": true}
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !drop[key] {
			env = append(env, item)
		}
	}
	return append(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1")
}

// 先頭の "*" で、ユーザー設定のMCPツールも含めて拒否する（後のルールが優先される）。
const readOnlyPermission = `{"*": "deny", "edit": "deny", "bash": "ask", "task": "deny", "doom_loop": "deny", "external_directory": "deny", "webfetch": "deny", "websearch": "deny", "read": {"*": "allow", "*.env": "deny", "*.env.*": "deny", "*.env.example": "allow"}, "glob": "allow", "grep": "allow", "list": "allow", "lsp": "allow", "todowrite": "allow"}`
const workspaceWritePermission = `{"doom_loop": "deny", "external_directory": "deny", "read": {"*": "allow", "*.env": "deny", "*.env.*": "deny", "*.env.example": "allow"}}`

func opencodeEnv(sandbox string) []string {
	policy := readOnlyPermission
	if sandbox == "workspace-write" {
		policy = workspaceWritePermission
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "OPENCODE_DISABLE_AUTOUPDATE" || key == "OPENCODE_PERMISSION" {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_PERMISSION="+policy)
	return env
}

func runOpenCode(ctx context.Context, o runOptions) (backendResult, error) {
	args := []string{o.Binary, "run", "--format", "json", "--dir", o.Workspace}
	if o.Continue {
		args = append(args, "-s", o.Session)
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	args = append(args, "--", o.Prompt)
	r := runCLI(ctx, args, o.Workspace, nil, opencodeEnv(o.Sandbox), time.Duration(o.Timeout)*time.Second)
	answer, session, detail, rejected := parseOpenCode(r.Stdout)
	if err := commandFailure("opencode", r, o.Timeout, detailIfExit(r, detail)); err != nil {
		return backendResult{}, err
	}
	permission := strings.Contains(strings.ToLower(r.Stderr+detail), "permission")
	if strings.TrimSpace(answer) == "" && (rejected || permission) {
		return backendResult{}, fmt.Errorf("opencode produced no answer: a tool call was blocked by the sandbox=%q permission policy", o.Sandbox)
	}
	if detail != "" {
		return backendResult{}, fmt.Errorf("opencode error: %s", detail)
	}
	if strings.TrimSpace(answer) == "" {
		return backendResult{}, fmt.Errorf("opencode produced no text in its JSON output")
	}
	return backendResult{Answer: strings.TrimSpace(answer), Session: session}, nil
}

func parseOpenCode(text string) (answer, session, detail string, rejected bool) {
	var chunks []string
	for _, line := range strings.Split(strings.ToValidUTF8(text, "�"), "\n") {
		var e map[string]json.RawMessage
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) != nil {
			continue
		}
		if s := rawString(e["sessionID"]); s != "" {
			session = s
		}
		switch rawString(e["type"]) {
		case "text":
			var p map[string]json.RawMessage
			_ = json.Unmarshal(e["part"], &p)
			if s := strings.TrimSpace(rawString(p["text"])); s != "" {
				chunks = append(chunks, s)
			}
		case "error":
			detail = opencodeError(e["error"])
		case "tool_use":
			if strings.Contains(strings.ToLower(string(e["part"])), "rejected") || strings.Contains(strings.ToLower(string(e["part"])), "permission") {
				rejected = true
			}
		}
	}
	return strings.Join(chunks, "\n\n"), session, detail, rejected
}

func opencodeError(raw json.RawMessage) string {
	var e map[string]json.RawMessage
	if json.Unmarshal(raw, &e) == nil {
		var d map[string]json.RawMessage
		if json.Unmarshal(e["data"], &d) == nil {
			if s := rawString(d["message"]); s != "" {
				return s
			}
		}
		if s := rawString(e["message"]); s != "" {
			return s
		}
		if s := rawString(e["name"]); s != "" {
			return s
		}
	}
	return rawString(raw)
}
