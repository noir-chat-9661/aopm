package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const quotaTimeout = 20 * time.Second

func quota(ctx context.Context, backend, binary string) string {
	switch backend {
	case "antigravity":
		return quotaAntigravity(ctx, binary)
	case "codex":
		return quotaCodex(ctx, binary)
	case "claude":
		return quotaClaude(ctx, binary)
	default:
		// copilot / opencode は公式の非対話手段がないため CLI を起動しない。
		return "not available"
	}
}

func quotaAntigravity(ctx context.Context, binary string) string {
	r := runCLI(ctx, []string{binary, "-p", "/usage", "--output-format", "json"}, os.TempDir(), nil, nil, quotaTimeout)
	if r.TimedOut {
		return "timed out"
	}
	if r.Cancelled {
		return "cancelled"
	}
	if r.StartErr != nil || r.ExitCode != 0 {
		return "not available"
	}
	obj, ok := findJSONObject(r.Stdout, "command")
	if !ok {
		return "not available"
	}
	var cmd struct {
		Data struct {
			Groups []struct {
				Name    string `json:"name"`
				Buckets []struct {
					Window            string   `json:"window"`
					RemainingFraction *float64 `json:"remaining_fraction"`
					ResetTime         string   `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	}
	if json.Unmarshal(obj["command"], &cmd) != nil {
		return "not available"
	}
	now := time.Now()
	var lines []string
	for _, g := range cmd.Data.Groups {
		gName := sanitizeText(g.Name)
		for _, b := range g.Buckets {
			if b.RemainingFraction == nil || *b.RemainingFraction < 0 || *b.RemainingFraction > 1 {
				continue
			}
			win := sanitizeText(b.Window)
			if win == "" {
				win = "quota"
			}
			label := win
			if gName != "" {
				label = gName + " " + win
			}
			var resetAt time.Time
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(b.ResetTime)); err == nil && t.Year() > 1970 {
				resetAt = t
			}
			lines = append(lines, formatQuotaLine(label, clampPercent(*b.RemainingFraction*100), resetAt, now))
		}
	}
	if len(lines) == 0 {
		return "not available"
	}
	return strings.Join(lines, "\n")
}

func quotaCodex(parent context.Context, binary string) string {
	ctx, cancel := context.WithTimeout(parent, quotaTimeout)
	defer cancel()

	cmd := exec.Command(binary, "app-server")
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 700 * time.Millisecond

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return "not available"
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return "not available"
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return "not available"
	}
	_ = stdoutW.Close()

	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	done := make(chan struct{})
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			close(done)
			_ = stdin.Close()
			signalGroup(pid, syscall.SIGTERM)
			timer := time.NewTimer(killGrace)
			select {
			case <-waitCh:
				timer.Stop()
			case <-timer.C:
				signalGroup(pid, syscall.SIGKILL)
				<-waitCh
			}
			signalGroup(pid, syscall.SIGKILL)
			_ = stdoutR.Close()
		})
	}
	defer cleanup()

	type readLine struct {
		line string
		err  error
	}
	lineCh := make(chan readLine, 16)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			select {
			case lineCh <- readLine{line: sc.Text()}:
			case <-done:
				return
			}
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case lineCh <- readLine{err: err}:
		case <-done:
		}
	}()

	waitID := func(wantID int) (json.RawMessage, string) {
		for {
			select {
			case <-ctx.Done():
				if ctx.Err() == context.DeadlineExceeded {
					return nil, "timed out"
				}
				return nil, "cancelled"
			case rl := <-lineCh:
				if rl.line != "" {
					var msg struct {
						ID     *int            `json:"id"`
						Result json.RawMessage `json:"result"`
						Error  json.RawMessage `json:"error"`
					}
					if json.Unmarshal([]byte(strings.TrimSpace(rl.line)), &msg) == nil && msg.ID != nil && *msg.ID == wantID {
						if len(msg.Error) > 0 && string(msg.Error) != "null" {
							return nil, "not available"
						}
						return msg.Result, ""
					}
				}
				if rl.err != nil {
					return nil, "not available"
				}
			}
		}
	}

	initReq, err := json.Marshal(map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "aopm",
				"title":   "aopm",
				"version": version,
			},
		},
	})
	if err != nil {
		return "not available"
	}
	if _, err := stdin.Write(append(initReq, '\n')); err != nil {
		return "not available"
	}
	if _, reason := waitID(1); reason != "" {
		return reason
	}
	if _, err := io.WriteString(stdin, "{\"method\":\"initialized\"}\n{\"id\":2,\"method\":\"account/rateLimits/read\"}\n"); err != nil {
		return "not available"
	}
	res, reason := waitID(2)
	if reason != "" {
		return reason
	}

	type codexWindow struct {
		UsedPercent        *float64 `json:"usedPercent"`
		WindowDurationMins *int     `json:"windowDurationMins"`
		ResetsAt           *int64   `json:"resetsAt"`
	}
	var payload struct {
		RateLimits struct {
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rateLimits"`
	}
	if json.Unmarshal(res, &payload) != nil {
		return "not available"
	}

	now := time.Now()
	var lines []string
	for _, entry := range []struct {
		name string
		w    *codexWindow
	}{
		{"primary", payload.RateLimits.Primary},
		{"secondary", payload.RateLimits.Secondary},
	} {
		w := entry.w
		if w == nil || w.UsedPercent == nil {
			continue
		}
		label := codexWindowLabel(entry.name, w.WindowDurationMins)
		var resetAt time.Time
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			t := time.Unix(*w.ResetsAt, 0)
			if t.Year() > 1970 {
				resetAt = t
			}
		}
		lines = append(lines, formatQuotaLine(label, clampPercent(100-*w.UsedPercent), resetAt, now))
	}
	if len(lines) == 0 {
		return "not available"
	}
	return strings.Join(lines, "\n")
}

func codexWindowLabel(fallback string, mins *int) string {
	if mins == nil || *mins <= 0 {
		return fallback
	}
	switch *mins {
	case 300:
		return "5h"
	case 10080:
		return "weekly"
	default:
		if *mins%1440 == 0 {
			return fmt.Sprintf("%dd", *mins/1440)
		}
		if *mins%60 == 0 {
			return fmt.Sprintf("%dh", *mins/60)
		}
		return fmt.Sprintf("%dm", *mins)
	}
}

var claudeUsageRE = regexp.MustCompile(`^Current\s+([^:]+):\s*([0-9]+(?:\.[0-9]+)?)%\s*used(?:\s*[·•|-]\s*resets\s+(.+))?$`)

func quotaClaude(ctx context.Context, binary string) string {
	args := []string{binary, "-p", "/usage", "--output-format", "json", "--no-session-persistence", "--strict-mcp-config", "--settings", `{"disableAllHooks":true}`, "--setting-sources", ""}
	r := runCLI(ctx, args, os.TempDir(), nil, claudeEnv(), quotaTimeout)
	if r.TimedOut {
		return "timed out"
	}
	if r.Cancelled {
		return "cancelled"
	}
	if r.StartErr != nil || r.ExitCode != 0 {
		return "not available"
	}
	resultText := extractClaudeResult(r.Stdout)
	if strings.TrimSpace(resultText) == "" {
		return "not available"
	}
	var lines []string
	for _, line := range strings.Split(stripANSI(resultText), "\n") {
		line = strings.TrimSpace(line)
		m := claudeUsageRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		used, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		label := claudeWindowLabel(sanitizeText(m[1]))
		pct := clampPercent(100 - used)
		resetRaw := sanitizeText(m[3])
		if resetRaw != "" {
			lines = append(lines, fmt.Sprintf("%s %d%% left (resets %s)", label, pct, resetRaw))
		} else {
			lines = append(lines, fmt.Sprintf("%s %d%% left", label, pct))
		}
	}
	if len(lines) == 0 {
		return "not available"
	}
	return strings.Join(lines, "\n")
}

func extractClaudeResult(stdout string) string {
	trimmed := strings.TrimSpace(strings.ToValidUTF8(stdout, ""))
	if strings.HasPrefix(trimmed, "[") {
		var arr []map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &arr) == nil {
			for _, obj := range arr {
				if rawString(obj["type"]) == "result" {
					return rawString(obj["result"])
				}
			}
		}
	}
	if obj, ok := findJSONObject(trimmed, "result"); ok {
		if t := rawString(obj["type"]); t == "" || t == "result" {
			return rawString(obj["result"])
		}
	}
	return ""
}

func claudeWindowLabel(name string) string {
	lower := strings.ToLower(name)
	switch {
	case lower == "session":
		return "5h"
	case lower == "week" || lower == "week (all models)":
		return "weekly"
	case strings.HasPrefix(lower, "week "):
		return "weekly " + strings.TrimSpace(name[5:])
	default:
		return name
	}
}

func clampPercent(v float64) int {
	p := int(math.Round(v))
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func formatQuotaLine(label string, remainingPct int, resetAt, now time.Time) string {
	line := fmt.Sprintf("%s %d%% left", label, remainingPct)
	if !resetAt.IsZero() && resetAt.Year() > 1970 {
		local := resetAt.In(time.Local)
		layout := "15:04"
		if local.Sub(now) >= 24*time.Hour {
			layout = "Jan 2 15:04"
		}
		line += " (resets " + local.Format(layout) + ")"
	}
	return line
}

func formatQuotaField(q string) string {
	return strings.ReplaceAll(q, "\n", "\n  ")
}

func sanitizeText(s string) string {
	s = stripANSI(strings.ToValidUTF8(s, ""))
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

type backendStatus struct {
	Name      string
	Installed bool
	Version   string
	Auth      string
	Quota     string
}

type statusUpdate struct {
	idx   int
	field string
	val   string
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if dn, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, dn) {
		return false
	}
	return true
}

func isLiveTTY(w io.Writer) bool {
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && isCharDevice(f)
}

func runCLIArgs(args []string, stdinIsTTY, stdoutIsTTY bool, stdout, stderr io.Writer) (handled bool, exitCode int) {
	if len(args) <= 1 {
		if !stdinIsTTY {
			return false, 0
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		printAllStatuses(ctx, stdout, stdoutIsTTY)
		if ctx.Err() != nil {
			return true, 130
		}
		exe, err := os.Executable()
		if err != nil || exe == "" {
			exe = "aopm"
		}
		fmt.Fprintf(stdout, "\nMCP server: run via an MCP client (e.g. mcpm add aopm -- %s)\n", exe)
		return true, 0
	}
	if len(args) == 2 {
		switch args[1] {
		case "status":
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			printAllStatuses(ctx, stdout, stdoutIsTTY)
			if ctx.Err() != nil {
				return true, 130
			}
			return true, 0
		case "--version", "version":
			fmt.Fprintf(stdout, "aopm %s\n", version)
			return true, 0
		case "--help", "-h", "help":
			printUsage(stdout)
			return true, 0
		}
	}
	printUsage(stderr)
	return true, 2
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: aopm [status | --version | --help]")
	fmt.Fprintln(w, "Run with no arguments on a pipe to start the MCP stdio server.")
}

func printAllStatuses(ctx context.Context, w io.Writer, live bool) {
	statuses := make([]backendStatus, len(backendNames))
	for i, name := range backendNames {
		statuses[i] = backendStatus{
			Name:      name,
			Installed: true,
			Version:   "fetching...",
			Auth:      "fetching...",
			Quota:     "fetching...",
		}
	}

	prevLines := 0
	if live {
		defer func() { _, _ = io.WriteString(w, "\x1b[?7h") }()
		writeLiveFrame(w, renderStatuses(statuses), &prevLines, true)
	}

	updates := make(chan statusUpdate, len(backendNames)*3)
	var wg sync.WaitGroup
	for i, b := range backendNames {
		wg.Add(1)
		go func(idx int, backend string) {
			defer wg.Done()
			bin, err := resolveBackendBinary(backend)
			if err != nil {
				updates <- statusUpdate{idx: idx, field: "missing"}
				return
			}
			var subWg sync.WaitGroup
			subWg.Add(3)
			go func() {
				defer subWg.Done()
				updates <- statusUpdate{idx: idx, field: "version", val: fetchVersion(ctx, backend, bin)}
			}()
			go func() {
				defer subWg.Done()
				updates <- statusUpdate{idx: idx, field: "auth", val: summarizeAuth(ctx, backend, bin)}
			}()
			go func() {
				defer subWg.Done()
				updates <- statusUpdate{idx: idx, field: "quota", val: quota(ctx, backend, bin)}
			}()
			subWg.Wait()
		}(i, b)
	}
	go func() {
		wg.Wait()
		close(updates)
	}()

	for u := range updates {
		switch u.field {
		case "missing":
			statuses[u.idx].Installed = false
		case "version":
			if u.val == "not installed" {
				statuses[u.idx].Installed = false
			} else {
				statuses[u.idx].Version = u.val
			}
		case "auth":
			statuses[u.idx].Auth = u.val
		case "quota":
			statuses[u.idx].Quota = u.val
		}
		if live {
			writeLiveFrame(w, renderStatuses(statuses), &prevLines, false)
		}
	}

	if ctx.Err() != nil {
		for i := range statuses {
			if statuses[i].Version == "fetching..." {
				statuses[i].Version = "cancelled"
			}
			if statuses[i].Auth == "fetching..." {
				statuses[i].Auth = "cancelled"
			}
			if statuses[i].Quota == "fetching..." {
				statuses[i].Quota = "cancelled"
			}
		}
		if live {
			writeLiveFrame(w, renderStatuses(statuses), &prevLines, false)
		}
	}
	if !live {
		_, _ = io.WriteString(w, renderStatuses(statuses))
	}
}

func renderStatuses(statuses []backendStatus) string {
	var buf bytes.Buffer
	for _, st := range statuses {
		if !st.Installed {
			fmt.Fprintf(&buf, "%-11s not installed\n", st.Name)
			continue
		}
		fmt.Fprintf(&buf, "%-11s %s  %s\n", st.Name, st.Version, st.Auth)
		switch st.Quota {
		case "fetching...", "not available", "timed out", "cancelled":
			fmt.Fprintf(&buf, "  quota %s\n", st.Quota)
		default:
			for _, ql := range strings.Split(st.Quota, "\n") {
				fmt.Fprintf(&buf, "  %s\n", ql)
			}
		}
	}
	return buf.String()
}

func writeLiveFrame(w io.Writer, frame string, prevLines *int, first bool) {
	var buf bytes.Buffer
	if first {
		buf.WriteString("\x1b[?7l")
	}
	if *prevLines > 0 {
		fmt.Fprintf(&buf, "\x1b[%dA\x1b[J", *prevLines)
	}
	buf.WriteString(frame)
	_, _ = w.Write(buf.Bytes())
	*prevLines = strings.Count(frame, "\n")
}

func fetchVersion(ctx context.Context, backend, binary string) string {
	r := runCLI(ctx, []string{binary, "--version"}, "", nil, nil, 15*time.Second)
	if r.Cancelled {
		return "cancelled"
	}
	if r.TimedOut {
		return "timed out"
	}
	if statusProcessError(backend, r) != nil {
		if r.StartErr != nil {
			return "not installed"
		}
		return "error"
	}
	return sanitizeText(firstLine(r.Stdout + "\n" + r.Stderr))
}

func summarizeAuth(ctx context.Context, backend, binary string) string {
	if backend == "copilot" {
		return "not verified"
	}
	authArgs := map[string][]string{
		"codex":       {"login", "status"},
		"claude":      {"auth", "status"},
		"antigravity": {"models"},
		"opencode":    {"auth", "list"},
	}
	r := runCLI(ctx, append([]string{binary}, authArgs[backend]...), "", nil, nil, 15*time.Second)
	if r.Cancelled {
		return "cancelled"
	}
	if r.TimedOut {
		return "timed out"
	}
	switch backend {
	case "antigravity":
		if r.ExitCode == 0 && sanitizeText(r.Stdout) != "" {
			return "logged in"
		}
		if r.ExitCode != 0 {
			return fmt.Sprintf("command exited %d", r.ExitCode)
		}
		return "not logged in"
	case "claude", "codex":
		if r.ExitCode == 0 {
			return "logged in"
		}
		return "not logged in"
	default:
		// opencode 等は終了コードだけで認証有効性を断定できない。
		if r.ExitCode != 0 {
			return fmt.Sprintf("command exited %d", r.ExitCode)
		}
		return "not verified"
	}
}
