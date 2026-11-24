package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	quotaTimeout          = 20 * time.Second
	opencodeResponseLimit = 64 << 10
)

var (
	opencodeUsageURL  = "https://opencode.ai/inference/go/v1/usage"
	opencodeTransport http.RoundTripper
)

type quotaMeter struct {
	Group     string
	Window    string
	Left      int
	Reset     time.Time
	ResetText string
}

func quota(ctx context.Context, backend, binary string) ([]quotaMeter, string) {
	var meters []quotaMeter
	var reason string
	switch backend {
	case "antigravity":
		meters, reason = quotaAntigravity(ctx, binary)
	case "codex":
		meters, reason = quotaCodex(ctx, binary)
	case "claude":
		meters, reason = quotaClaude(ctx, binary)
	case "opencode":
		meters, reason = quotaOpenCode(ctx)
	default:
		// copilot は公式の非対話手段がないため CLI を起動しない。
		return nil, "not available"
	}
	if reason == "" && len(meters) > 1 {
		sortQuotaMeters(meters)
	}
	return meters, reason
}

func windowRank(win string) int {
	switch win {
	case "5h":
		return 0
	case "weekly":
		return 1
	case "monthly":
		return 2
	default:
		return 3
	}
}

func sortQuotaMeters(meters []quotaMeter) {
	groupOrder := make(map[string]int, len(meters))
	for _, m := range meters {
		if _, ok := groupOrder[m.Group]; !ok {
			groupOrder[m.Group] = len(groupOrder)
		}
	}
	sort.SliceStable(meters, func(i, j int) bool {
		gi, gj := groupOrder[meters[i].Group], groupOrder[meters[j].Group]
		if gi != gj {
			return gi < gj
		}
		return windowRank(meters[i].Window) < windowRank(meters[j].Window)
	})
}

func quotaAntigravity(ctx context.Context, binary string) ([]quotaMeter, string) {
	r := runCLI(ctx, []string{binary, "-p", "/usage", "--output-format", "json"}, os.TempDir(), nil, nil, quotaTimeout)
	if r.TimedOut {
		return nil, "timed out"
	}
	if r.Cancelled {
		return nil, "cancelled"
	}
	if r.StartErr != nil || r.ExitCode != 0 {
		return nil, "not available"
	}
	obj, ok := findJSONObject(r.Stdout, "command")
	if !ok {
		return nil, "not available"
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
		return nil, "not available"
	}
	var meters []quotaMeter
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
			var resetAt time.Time
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(b.ResetTime)); err == nil && t.Year() > 1970 {
				resetAt = t
			}
			meters = append(meters, quotaMeter{
				Group:  gName,
				Window: win,
				Left:   clampPercent(*b.RemainingFraction * 100),
				Reset:  resetAt,
			})
		}
	}
	if len(meters) == 0 {
		return nil, "not available"
	}
	return meters, ""
}

func quotaCodex(parent context.Context, binary string) ([]quotaMeter, string) {
	ctx, cancel := context.WithTimeout(parent, quotaTimeout)
	defer cancel()

	cmd := exec.Command(binary, "app-server")
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 700 * time.Millisecond

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, "not available"
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, "not available"
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, "not available"
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
		return nil, "not available"
	}
	if _, err := stdin.Write(append(initReq, '\n')); err != nil {
		return nil, "not available"
	}
	if _, reason := waitID(1); reason != "" {
		return nil, reason
	}
	if _, err := io.WriteString(stdin, "{\"method\":\"initialized\"}\n{\"id\":2,\"method\":\"account/rateLimits/read\"}\n"); err != nil {
		return nil, "not available"
	}
	res, reason := waitID(2)
	if reason != "" {
		return nil, reason
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
		return nil, "not available"
	}

	var meters []quotaMeter
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
		win := codexWindowLabel(entry.name, w.WindowDurationMins)
		var resetAt time.Time
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			t := time.Unix(*w.ResetsAt, 0)
			if t.Year() > 1970 {
				resetAt = t
			}
		}
		meters = append(meters, quotaMeter{
			Window: win,
			Left:   clampPercent(100 - *w.UsedPercent),
			Reset:  resetAt,
		})
	}
	if len(meters) == 0 {
		return nil, "not available"
	}
	return meters, ""
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

func quotaClaude(ctx context.Context, binary string) ([]quotaMeter, string) {
	args := []string{binary, "-p", "/usage", "--output-format", "json", "--no-session-persistence", "--strict-mcp-config", "--settings", `{"disableAllHooks":true}`, "--setting-sources", ""}
	r := runCLI(ctx, args, os.TempDir(), nil, claudeEnv(), quotaTimeout)
	if r.TimedOut {
		return nil, "timed out"
	}
	if r.Cancelled {
		return nil, "cancelled"
	}
	if r.StartErr != nil || r.ExitCode != 0 {
		return nil, "not available"
	}
	resultText := extractClaudeResult(r.Stdout)
	if strings.TrimSpace(resultText) == "" {
		return nil, "not available"
	}
	now := time.Now()
	var meters []quotaMeter
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
		win := claudeWindowLabel(sanitizeText(m[1]))
		pct := clampPercent(100 - used)
		resetAt, resetText := parseClaudeReset(m[3], now)
		meters = append(meters, quotaMeter{
			Window:    win,
			Left:      pct,
			Reset:     resetAt,
			ResetText: resetText,
		})
	}
	if len(meters) == 0 {
		return nil, "not available"
	}
	return meters, ""
}

func parseClaudeReset(raw string, now time.Time) (time.Time, string) {
	clean := sanitizeText(raw)
	if clean == "" {
		return time.Time{}, ""
	}
	loc := now.Location()
	datePart := clean
	if strings.HasSuffix(clean, ")") {
		if idx := strings.LastIndex(clean, "("); idx >= 0 {
			tzName := strings.TrimSpace(clean[idx+1 : len(clean)-1])
			datePart = strings.TrimSpace(clean[:idx])
			if tzName == "" {
				return time.Time{}, clean
			}
			loaded, err := time.LoadLocation(tzName)
			if err != nil {
				return time.Time{}, clean
			}
			loc = loaded
		}
	}
	for _, layout := range []string{"Jan 2 at 3:04pm", "Jan 2 at 3pm"} {
		parsed, err := time.ParseInLocation(layout, datePart, loc)
		if err == nil {
			nowInLoc := now.In(loc)
			t := time.Date(nowInLoc.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
			if t.Before(nowInLoc.AddDate(0, -6, 0)) {
				t = t.AddDate(1, 0, 0)
			}
			return t, ""
		}
	}
	return time.Time{}, clean
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

func quotaOpenCode(parent context.Context) ([]quotaMeter, string) {
	key, ok := readOpenCodeGoKey()
	if !ok {
		return nil, "not available"
	}
	ctx, cancel := context.WithTimeout(parent, quotaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencodeUsageURL, nil)
	if err != nil {
		return nil, "not available"
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client := &http.Client{
		Timeout:   quotaTimeout,
		Transport: opencodeTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, "timed out"
		}
		if ctx.Err() == context.Canceled {
			return nil, "cancelled"
		}
		return nil, "not available"
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "not available"
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, opencodeResponseLimit+1))
	if err != nil || len(body) > opencodeResponseLimit {
		return nil, "not available"
	}

	type openCodeEntry struct {
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resetsAt"`
	}
	var payload struct {
		Usage struct {
			Rolling *openCodeEntry `json:"rolling"`
			Weekly  *openCodeEntry `json:"weekly"`
			Monthly *openCodeEntry `json:"monthly"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil, "not available"
	}

	var meters []quotaMeter
	for _, item := range []struct {
		win   string
		entry *openCodeEntry
	}{
		{"5h", payload.Usage.Rolling},
		{"weekly", payload.Usage.Weekly},
		{"monthly", payload.Usage.Monthly},
	} {
		if item.entry == nil || item.entry.Percent == nil {
			continue
		}
		var resetAt time.Time
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(item.entry.ResetsAt)); err == nil && t.Year() > 1970 {
			resetAt = t
		}
		meters = append(meters, quotaMeter{
			Window: item.win,
			Left:   clampPercent(100 - *item.entry.Percent),
			Reset:  resetAt,
		})
	}
	if len(meters) == 0 {
		return nil, "not available"
	}
	return meters, ""
}

func readOpenCodeGoKey() (string, bool) {
	root := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		root = filepath.Join(home, ".local", "share")
	}
	b, err := os.ReadFile(filepath.Join(root, "opencode", "auth.json"))
	if err != nil {
		return "", false
	}
	var auth struct {
		OpenCodeGo *struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		} `json:"opencode-go"`
	}
	if json.Unmarshal(b, &auth) != nil || auth.OpenCodeGo == nil || auth.OpenCodeGo.Type != "api" {
		return "", false
	}
	if !validBearerKey(auth.OpenCodeGo.Key) {
		return "", false
	}
	return auth.OpenCodeGo.Key, true
}

func validBearerKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return false
		}
	}
	return true
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

func formatQuotaMCP(meters []quotaMeter, reason string, now time.Time) string {
	if reason != "" {
		return reason
	}
	if len(meters) == 0 {
		return "not available"
	}
	lines := make([]string, 0, len(meters))
	for _, m := range meters {
		label := m.Window
		if m.Group != "" {
			label = m.Group + " " + m.Window
		}
		line := fmt.Sprintf("%s %d%% left", label, m.Left)
		if !m.Reset.IsZero() && m.Reset.Year() > 1970 {
			local := m.Reset.In(time.Local)
			layout := "15:04"
			if local.Sub(now) >= 24*time.Hour {
				layout = "Jan 2 15:04"
			}
			line += " (resets " + local.Format(layout) + ")"
		} else if m.ResetText != "" {
			line += " (resets " + m.ResetText + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
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

var versionNumRE = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

func extractVersion(raw string) string {
	s := sanitizeText(raw)
	if m := versionNumRE.FindString(s); m != "" {
		return m
	}
	return s
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
	return extractVersion(firstLine(r.Stdout + "\n" + r.Stderr))
}

func summarizeAuth(ctx context.Context, backend, binary string) string {
	if backend == "copilot" || backend == "opencode" {
		return "not verified"
	}
	authArgs := map[string][]string{
		"codex":       {"login", "status"},
		"claude":      {"auth", "status"},
		"antigravity": {"models"},
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
		return "not verified"
	}
}
