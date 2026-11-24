package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var backendNames = []string{"antigravity", "codex", "copilot", "claude", "opencode"}
var actionNames = []string{"ask", "continue", "status"}

type callArgs struct {
	Prompt    string
	Workspace string
	Model     string
	Sandbox   *string
	Effort    string
	Timeout   int
	Plan      bool
	Schema    json.RawMessage
}

type runOptions struct {
	Backend   string
	Binary    string
	Prompt    string
	Workspace string
	Model     string
	Sandbox   string
	Effort    string
	Timeout   int
	Continue  bool
	Session   string
	Plan      bool
	Schema    json.RawMessage
}

type backendResult struct {
	Answer  string
	Session string
}

type argumentError string

func (e argumentError) Error() string { return string(e) }

type sessionPin struct {
	Session string `json:"session"`
	Sandbox string `json:"sandbox"`
}

const defaultTimeout = 600

func knownTool(name string) bool {
	for _, b := range backendNames {
		for _, a := range actionNames {
			if name == b+"_"+a {
				return true
			}
		}
	}
	return false
}

func toolDefinitions() []map[string]any {
	var out []map[string]any
	for _, b := range backendNames {
		for _, action := range []string{"ask", "continue", "status"} {
			name := b + "_" + action
			readOnly := action == "status"
			properties := map[string]any{}
			required := []string{}
			if action != "status" {
				properties["prompt"] = map[string]any{"type": "string", "description": "Prompt for the agent."}
				properties["workspace"] = map[string]any{"type": "string", "description": "Absolute existing project directory."}
				properties["model"] = map[string]any{"type": "string"}
				properties["timeout_s"] = map[string]any{"type": "integer", "default": defaultTimeout, "minimum": 1, "maximum": 7200}
				required = []string{"prompt", "workspace"}
				properties["sandbox"] = map[string]any{"type": "string", "enum": []string{"read-only", "workspace-write"}}
				if action == "ask" {
					properties["sandbox"].(map[string]any)["default"] = "read-only"
				}
				switch b {
				case "codex":
					properties["effort"] = map[string]any{"type": "string", "enum": []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}}
				case "claude":
					properties["effort"] = map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "xhigh", "max"}}
				case "copilot":
					properties["effort"] = map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "xhigh", "max"}}
				case "antigravity":
					properties["effort"] = map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "xhigh", "max"}}
					properties["plan"] = map[string]any{"type": "boolean", "default": false}
					properties["schema"] = map[string]any{"description": "JSON Schema object or JSON text.", "oneOf": []map[string]any{{"type": "object"}, {"type": "string"}}}
				}
			}
			ann := map[string]any{"readOnlyHint": readOnly, "openWorldHint": !readOnly}
			out = append(out, map[string]any{
				"name":        name,
				"description": toolDescription(b, action),
				"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
				"annotations": ann,
			})
		}
	}
	return out
}

func toolDescription(backend, action string) string {
	if action == "status" {
		return "Check the installed " + backend + " CLI and authentication without using model quota."
	}
	detail := map[string]string{
		"antigravity": "Antigravity/Gemini; read-only runs no shell commands (include any diff in the prompt), workspace-write runs them in an OS sandbox.",
		"codex":       "OpenAI Codex with a real OS sandbox (MCP servers in the codex config run outside it).",
		"copilot":     "GitHub Copilot with agent-enforced permissions.",
		"claude":      "Claude Code with agent-enforced permissions.",
		"opencode":    "opencode with agent-enforced permissions.",
	}[backend]
	return strings.Title(action) + " using " + detail + " Always pass workspace and use trusted prompts."
}

func parseToolArgs(name string, raw json.RawMessage) (callArgs, error) {
	var a callArgs
	raw = bytes.TrimSpace(raw)
	var fields map[string]json.RawMessage
	if omitted(raw) {
		fields = map[string]json.RawMessage{}
	} else if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return a, fmt.Errorf("arguments for %s must be an object", name)
	}
	backend, action, ok := splitTool(name)
	if !ok {
		return a, fmt.Errorf("unknown tool: %s", name)
	}
	allowed := map[string]bool{"prompt": true, "workspace": true, "model": true, "timeout_s": true, "sandbox": true, "watch": true}
	if backend != "opencode" {
		allowed["effort"] = true
	}
	if backend == "antigravity" {
		allowed["plan"], allowed["schema"] = true, true
	}
	if action == "status" {
		allowed = map[string]bool{"watch": true}
	}
	for key := range fields {
		if !allowed[key] {
			return a, fmt.Errorf("unknown argument %q for %s", key, name)
		}
	}
	if action == "status" {
		if v, ok := fields["watch"]; ok && !omitted(v) {
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return a, fmt.Errorf("watch must be a boolean")
			}
		}
		return a, nil
	}
	var err error
	a.Prompt, err = requiredString(fields, "prompt")
	if err != nil {
		return a, err
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return a, fmt.Errorf("prompt must be nonblank")
	}
	a.Workspace, err = requiredString(fields, "workspace")
	if err != nil {
		return a, err
	}
	if strings.TrimSpace(a.Workspace) == "" {
		return a, fmt.Errorf("workspace must be nonblank")
	}
	a.Model, err = optionalString(fields, "model")
	if err != nil {
		return a, err
	}
	a.Effort, err = optionalString(fields, "effort")
	if err != nil {
		return a, err
	}
	if a.Effort != "" {
		valid := map[string]bool{}
		if backend == "codex" {
			for _, v := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
				valid[v] = true
			}
		} else {
			for _, v := range []string{"low", "medium", "high", "xhigh", "max"} {
				valid[v] = true
			}
		}
		if !valid[a.Effort] {
			return a, fmt.Errorf("invalid effort %q", a.Effort)
		}
	}
	a.Timeout = defaultTimeout
	if v, ok := fields["timeout_s"]; ok && !omitted(v) {
		if json.Unmarshal(v, &a.Timeout) != nil {
			return a, fmt.Errorf("timeout_s must be an integer")
		}
	}
	if a.Timeout < 1 || a.Timeout > 7200 {
		return a, fmt.Errorf("timeout_s must be between 1 and 7200")
	}
	if v, ok := fields["sandbox"]; ok && !omitted(v) {
		var mode string
		if json.Unmarshal(v, &mode) != nil || (mode != "read-only" && mode != "workspace-write") {
			return a, fmt.Errorf("sandbox must be read-only or workspace-write")
		}
		a.Sandbox = &mode
	}
	if backend == "antigravity" {
		if v, ok := fields["plan"]; ok && !omitted(v) {
			if json.Unmarshal(v, &a.Plan) != nil {
				return a, fmt.Errorf("plan must be a boolean")
			}
		}
		if v, ok := fields["schema"]; ok && !omitted(v) {
			a.Schema, err = normalizeSchema(v)
			if err != nil {
				return a, err
			}
		}
	}
	if v, ok := fields["watch"]; ok && !omitted(v) {
		var b bool
		if json.Unmarshal(v, &b) != nil {
			return a, fmt.Errorf("watch must be a boolean")
		}
	}
	return a, nil
}

func splitTool(name string) (string, string, bool) {
	for _, b := range backendNames {
		prefix := b + "_"
		if strings.HasPrefix(name, prefix) {
			a := strings.TrimPrefix(name, prefix)
			for _, action := range actionNames {
				if a == action {
					return b, a, true
				}
			}
		}
	}
	return "", "", false
}

func requiredString(m map[string]json.RawMessage, key string) (string, error) {
	v, ok := m[key]
	if !ok || omitted(v) {
		return "", fmt.Errorf("%s is required", key)
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

func optionalString(m map[string]json.RawMessage, key string) (string, error) {
	v, ok := m[key]
	if !ok || omitted(v) {
		return "", nil
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

func normalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	var obj map[string]json.RawMessage
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil || json.Unmarshal([]byte(text), &obj) != nil || obj == nil {
			return nil, fmt.Errorf("schema must be a JSON object or JSON text")
		}
		raw = json.RawMessage(text)
	} else if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("schema must be a JSON object or JSON text")
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return nil, fmt.Errorf("invalid schema")
	}
	return json.RawMessage(buf.Bytes()), nil
}

func normalizeWorkspace(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "~" || strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve workspace home: %w", err)
		}
		if s == "~" {
			s = home
		} else {
			s = filepath.Join(home, strings.TrimPrefix(s, "~/"))
		}
	}
	if !filepath.IsAbs(s) {
		return "", fmt.Errorf("workspace must be an absolute existing directory")
	}
	canonical, err := filepath.EvalSymlinks(s)
	if err != nil {
		return "", fmt.Errorf("workspace is not an existing directory: %w", err)
	}
	st, err := os.Stat(canonical)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace is not an existing directory")
	}
	return canonical, nil
}

func resolveBinary(envName, command string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		if strings.ContainsRune(v, filepath.Separator) {
			return filepath.Abs(v)
		}
		return v, nil
	}
	if p, err := exec.LookPath(command); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, ".local", "bin", command), filepath.Join(home, ".opencode", "bin", command), filepath.Join("/opt/homebrew/bin", command), filepath.Join("/usr/local/bin", command)} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not installed (set %s or put %s on PATH)", command, envName, command)
}

func uuid4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func executeTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	a, err := parseToolArgs(name, raw)
	if err != nil {
		return "", argumentError(err.Error())
	}
	backend, action, _ := splitTool(name)
	if action == "status" {
		bin, err := resolveBackendBinary(backend)
		if err != nil {
			return "", err
		}
		return runStatus(ctx, backend, bin)
	}
	workspace, err := normalizeWorkspace(a.Workspace)
	if err != nil {
		return "", argumentError(err.Error())
	}
	var pin sessionPin
	var found bool
	if action == "continue" {
		pin, found, err = loadSession(backend, workspace)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("no saved %s session for this workspace; call %s_ask first", backend, backend)
		}
	}
	sandbox := "read-only"
	if action == "continue" {
		sandbox = pin.Sandbox
	}
	if a.Sandbox != nil {
		sandbox = *a.Sandbox
	}
	if a.Plan {
		sandbox = "read-only"
	}
	bin, err := resolveBackendBinary(backend)
	if err != nil {
		return "", err
	}
	session := ""
	if action == "continue" {
		session = pin.Session
	}
	if backend == "copilot" && action == "ask" {
		session, err = uuid4()
		if err != nil {
			return "", fmt.Errorf("cannot create copilot session id: %w", err)
		}
	}
	r, err := runBackend(ctx, runOptions{Backend: backend, Binary: bin, Prompt: a.Prompt, Workspace: workspace, Model: a.Model, Sandbox: sandbox, Effort: a.Effort, Timeout: a.Timeout, Continue: action == "continue", Session: session, Plan: a.Plan, Schema: a.Schema})
	if err != nil {
		if r.Session != "" && ctx.Err() == nil {
			if saveErr := saveSession(backend, workspace, sessionPin{Session: r.Session, Sandbox: sandbox}); saveErr != nil {
				err = fmt.Errorf("%w (could not persist the session: %v)", err, saveErr)
			}
		}
		return "", err
	}
	if r.Session == "" {
		r.Session = session
	}
	// キャンセル済みの回答は返らないので、以前のセッションを上書きしない。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.Session == "" {
		// 古いセッションを誤って再開しないよう、保存済みの記録を消す。
		_ = os.Remove(sessionPath(backend, workspace))
		r.Answer += "\nWarning: the CLI returned no session id; this answer cannot be continued."
		return r.Answer, nil
	}
	if err := saveSession(backend, workspace, sessionPin{Session: r.Session, Sandbox: sandbox}); err != nil {
		r.Answer += "\nWarning: could not persist the session: " + err.Error()
	}
	return r.Answer, nil
}

func resolveBackendBinary(backend string) (string, error) {
	vars := map[string]string{"antigravity": "AGY_BIN", "codex": "CODEX_BIN", "copilot": "COPILOT_BIN", "claude": "CLAUDE_BIN", "opencode": "OPENCODE_BIN"}
	commands := map[string]string{"antigravity": "agy", "codex": "codex", "copilot": "copilot", "claude": "claude", "opencode": "opencode"}
	return resolveBinary(vars[backend], commands[backend])
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func runStatus(ctx context.Context, backend, binary string) (string, error) {
	r := runCLI(ctx, []string{binary, "--version"}, "", nil, nil, 15*time.Second)
	if err := statusProcessError(backend, r); err != nil {
		return "", err
	}
	version := firstLine(stripANSI(r.Stdout + "\n" + r.Stderr))
	meters, qReason := quota(ctx, backend, binary)
	auth := "not verified"
	switch backend {
	case "copilot":
	case "opencode":
		if qReason == "" && len(meters) > 0 {
			auth = "logged in"
		}
	default:
		authArgs := map[string][]string{"codex": {"login", "status"}, "claude": {"auth", "status"}, "antigravity": {"models"}}
		r = runCLI(ctx, append([]string{binary}, authArgs[backend]...), "", nil, nil, 15*time.Second)
		text := strings.TrimSpace(stripANSI(r.Stdout + "\n" + r.Stderr))
		if r.TimedOut {
			auth = "timed out"
		} else if backend == "claude" || backend == "codex" {
			// 生の出力はメールアドレス・組織名・マスク済みAPIキーを含みうるため返さない。
			auth = "not logged in"
			if r.ExitCode == 0 {
				auth = "logged in"
			}
		} else if text != "" {
			auth = text
		} else if r.ExitCode != 0 {
			auth = fmt.Sprintf("command exited %d", r.ExitCode)
		} else {
			auth = "no output"
		}
	}
	q := formatQuotaMCP(meters, qReason, time.Now())
	return fmt.Sprintf("binary: %s\nversion: %s\nauth: %s\nquota: %s", binary, version, auth, formatQuotaField(q)), nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return "(no version output)"
}

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func omitted(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}
