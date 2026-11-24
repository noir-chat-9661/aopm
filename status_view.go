package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

const statusMaxWidth = 80

var statusOrder = []string{"antigravity", "codex", "claude", "opencode", "copilot"}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

var getTerminalSize = defaultTerminalSize

func defaultTerminalSize(w io.Writer) (int, int) {
	f, ok := w.(*os.File)
	if !ok || !isCharDevice(f) {
		return 24, statusMaxWidth
	}
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Row == 0 {
		return 24, statusMaxWidth
	}
	cols := int(ws.Col)
	if cols <= 0 {
		cols = statusMaxWidth
	}
	return int(ws.Row), cols
}

type backendStatus struct {
	Name        string
	Installed   bool
	VersionDone bool
	Version     string
	AuthDone    bool
	Auth        string
	QuotaDone   bool
	Meters      []quotaMeter
	QuotaReason string
}

type statusUpdate struct {
	idx         int
	field       string
	val         string
	meters      []quotaMeter
	quotaReason string
}

type statusRenderOpts struct {
	Now   time.Time
	JA    bool
	Color bool
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

func isJapaneseLocale() bool {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return strings.HasPrefix(strings.ToLower(v), "ja")
		}
	}
	return false
}

func colorEnabled(live bool) bool {
	return live && os.Getenv("NO_COLOR") == ""
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
	opts := statusRenderOpts{
		Now:   time.Now(),
		JA:    isJapaneseLocale(),
		Color: colorEnabled(live),
	}

	statuses := make([]backendStatus, len(statusOrder))
	for i, name := range statusOrder {
		statuses[i] = backendStatus{
			Name:      name,
			Installed: true,
		}
	}

	prevLines := 0
	firstWrite := true
	compactMode := false
	fetchingLine := " fetching…"
	if opts.JA {
		fetchingLine = " 取得中…"
	}

	emitFrame := func(frame string) {
		if compactMode {
			return
		}
		rows, _ := getTerminalSize(w)
		if rows <= 0 {
			rows = 24
		}
		lines := strings.Count(frame, "\n")
		var buf bytes.Buffer
		if firstWrite {
			buf.WriteString("\x1b[?7l")
			firstWrite = false
		}
		if lines+1 > rows {
			compactMode = true
			if prevLines > 0 {
				fmt.Fprintf(&buf, "\x1b[%dA\x1b[J", prevLines)
				prevLines = 0
			}
			buf.WriteString(fetchingLine)
			_, _ = w.Write(buf.Bytes())
			return
		}
		if prevLines > 0 {
			fmt.Fprintf(&buf, "\x1b[%dA\x1b[J", prevLines)
		}
		buf.WriteString(frame)
		_, _ = w.Write(buf.Bytes())
		prevLines = lines
	}

	if live {
		defer func() { _, _ = io.WriteString(w, "\x1b[?7h") }()
		emitFrame(renderStatuses(statuses, opts))
	}

	updates := make(chan statusUpdate, len(statusOrder)*3)
	var wg sync.WaitGroup
	for i, b := range statusOrder {
		wg.Add(1)
		go func(idx int, backend string) {
			defer wg.Done()
			bin, err := resolveBackendBinary(backend)
			if err != nil {
				updates <- statusUpdate{idx: idx, field: "missing"}
				return
			}
			var subWg sync.WaitGroup
			subWg.Add(1)
			go func() {
				defer subWg.Done()
				updates <- statusUpdate{idx: idx, field: "version", val: fetchVersion(ctx, backend, bin)}
			}()
			switch backend {
			case "copilot":
				updates <- statusUpdate{idx: idx, field: "auth", val: "not verified"}
				updates <- statusUpdate{idx: idx, field: "quota", quotaReason: "not available"}
			case "opencode":
				subWg.Add(1)
				go func() {
					defer subWg.Done()
					meters, reason := quota(ctx, backend, bin)
					updates <- statusUpdate{idx: idx, field: "quota", meters: meters, quotaReason: reason}
				}()
			default:
				subWg.Add(2)
				go func() {
					defer subWg.Done()
					updates <- statusUpdate{idx: idx, field: "auth", val: summarizeAuth(ctx, backend, bin)}
				}()
				go func() {
					defer subWg.Done()
					meters, reason := quota(ctx, backend, bin)
					updates <- statusUpdate{idx: idx, field: "quota", meters: meters, quotaReason: reason}
				}()
			}
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
			statuses[u.idx].VersionDone = true
			statuses[u.idx].AuthDone = true
			statuses[u.idx].QuotaDone = true
		case "version":
			statuses[u.idx].VersionDone = true
			if u.val == "not installed" {
				statuses[u.idx].Installed = false
				statuses[u.idx].AuthDone = true
				statuses[u.idx].QuotaDone = true
			} else {
				statuses[u.idx].Version = u.val
			}
		case "auth":
			statuses[u.idx].AuthDone = true
			statuses[u.idx].Auth = u.val
		case "quota":
			statuses[u.idx].QuotaDone = true
			statuses[u.idx].Meters = u.meters
			statuses[u.idx].QuotaReason = u.quotaReason
			if statuses[u.idx].Name == "opencode" {
				statuses[u.idx].AuthDone = true
				if u.quotaReason == "" && len(u.meters) > 0 {
					statuses[u.idx].Auth = "logged in"
				} else if u.quotaReason == "cancelled" {
					statuses[u.idx].Auth = "cancelled"
				} else {
					statuses[u.idx].Auth = "not verified"
				}
			}
		}
		if live {
			opts.Now = time.Now()
			emitFrame(renderStatuses(statuses, opts))
		}
	}

	if ctx.Err() != nil {
		for i := range statuses {
			if !statuses[i].VersionDone {
				statuses[i].Version = "cancelled"
				statuses[i].VersionDone = true
			}
			if !statuses[i].AuthDone {
				statuses[i].Auth = "cancelled"
				statuses[i].AuthDone = true
			}
			if !statuses[i].QuotaDone {
				statuses[i].QuotaReason = "cancelled"
				statuses[i].QuotaDone = true
			}
		}
		if live {
			opts.Now = time.Now()
			if !compactMode {
				emitFrame(renderStatuses(statuses, opts))
			}
		}
	}

	opts.Now = time.Now()
	finalFrame := renderStatuses(statuses, opts)
	if live {
		if compactMode {
			_, _ = io.WriteString(w, "\r\x1b[K"+finalFrame)
		}
		return
	}
	_, _ = io.WriteString(w, finalFrame)
}

func renderStatuses(statuses []backendStatus, opts statusRenderOpts) string {
	winW := 5
	if opts.JA {
		winW = 2
	}
	for _, st := range statuses {
		for _, m := range st.Meters {
			w := displayWidth(translateWindow(m.Window, opts.JA))
			if w > winW {
				winW = w
			}
		}
	}
	if winW > 12 {
		winW = 12
	}

	blocks := make([]string, 0, len(statuses))
	for _, st := range statuses {
		blocks = append(blocks, renderBackendBlock(st, winW, opts))
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

func renderBackendBlock(st backendStatus, winW int, opts statusRenderOpts) string {
	name := displayBackendName(st.Name)
	nameCol := " " + padRightWidth(name, 10)

	if !st.VersionDone {
		msg := "fetching…"
		if opts.JA {
			msg = "取得中…"
		}
		return nameCol + msg
	}
	if !st.Installed {
		reason := "not-installed"
		if opts.JA {
			reason = "未導入"
		}
		return nameCol + colorIcon("✕", "\x1b[31m", opts.Color) + " " + reason
	}

	icon, iconColor, authReason := authDisplay(st.Auth, opts.JA)
	ver := truncateWidth(st.Version, 24)
	if ver == "" {
		ver = "-"
	}
	headerPlain := nameCol + icon + " " + ver
	headerColored := nameCol + colorIcon(icon, iconColor, opts.Color) + " " + ver

	if !st.QuotaDone {
		msg := "fetching…"
		if opts.JA {
			msg = "取得中…"
		}
		return headerColored + "\n   " + msg
	}

	if len(st.Meters) == 0 {
		if authReason != "" {
			maxReason := statusMaxWidth - displayWidth(headerPlain) - 2
			if maxReason > 0 {
				return headerColored + "  " + truncateWidth(authReason, maxReason)
			}
			return headerColored
		}
		msg := localizedQuotaReason(st.QuotaReason, opts.JA)
		pad := 40 - displayWidth(headerPlain)
		if pad < 2 {
			pad = 2
		}
		maxMsg := statusMaxWidth - displayWidth(headerPlain) - pad
		if maxMsg > 0 {
			return headerColored + strings.Repeat(" ", pad) + truncateWidth(msg, maxMsg)
		}
		return headerColored
	}

	var lines []string
	lines = append(lines, headerColored)
	prevGroup := ""
	for i, m := range st.Meters {
		gLabel := ""
		if i == 0 || m.Group != prevGroup {
			gLabel = translateGroup(m.Group, opts.JA)
		}
		prevGroup = m.Group
		groupCol := padRightWidth(truncateWidth(gLabel, 9), 10)
		winCol := padRightWidth(truncateWidth(translateWindow(m.Window, opts.JA), winW), winW) + "  "
		bar := renderBar(m.Left, opts.Color)
		pctText := fmt.Sprintf("  %3d%%", m.Left)
		if opts.Color {
			pctText = colorForLeft(m.Left) + pctText + "\x1b[0m"
		}
		line := "   " + groupCol + winCol + bar + pctText
		prefixW := 3 + 10 + winW + 2 + 20 + 6
		if rem := formatRemaining(m.Reset, m.ResetText, opts.Now, opts.JA); rem != "" {
			avail := statusMaxWidth - prefixW - 3
			if avail > 0 {
				line += "   " + truncateWidth(rem, avail)
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func displayBackendName(backend string) string {
	if backend == "antigravity" {
		return "agy"
	}
	return backend
}

func translateGroup(group string, ja bool) string {
	switch group {
	case "Gemini Models":
		return "Gemini"
	case "Claude and GPT models":
		if ja {
			return "他社"
		}
		return "Other"
	default:
		return group
	}
}

func translateWindow(win string, ja bool) string {
	switch win {
	case "weekly":
		if ja {
			return "週"
		}
		return "week"
	case "monthly":
		if ja {
			return "月"
		}
		return "month"
	default:
		return win
	}
}

func authDisplay(auth string, ja bool) (icon, colorCode, reason string) {
	switch auth {
	case "logged in":
		return "●", "\x1b[32m", ""
	case "not verified", "":
		return "○", "\x1b[90m", ""
	case "not logged in":
		if ja {
			return "✕", "\x1b[31m", "未ログイン"
		}
		return "✕", "\x1b[31m", "unauthenticated"
	case "cancelled":
		if ja {
			return "✕", "\x1b[31m", "中止"
		}
		return "✕", "\x1b[31m", "cancelled"
	case "timed out":
		if ja {
			return "○", "\x1b[90m", "時間切れ"
		}
		return "○", "\x1b[90m", "timed-out"
	default:
		if ja {
			return "✕", "\x1b[31m", "エラー"
		}
		return "✕", "\x1b[31m", "error"
	}
}

func localizedQuotaReason(reason string, ja bool) string {
	switch reason {
	case "timed out":
		if ja {
			return "時間切れ"
		}
		return "timed out"
	case "cancelled":
		if ja {
			return "中止"
		}
		return "cancelled"
	default:
		if ja {
			return "残量は取得できません"
		}
		return "quota unavailable"
	}
}

func colorIcon(icon, colorCode string, useColor bool) string {
	if !useColor {
		return icon
	}
	return colorCode + icon + "\x1b[0m"
}

func colorForLeft(left int) string {
	switch {
	case left >= 50:
		return "\x1b[32m"
	case left >= 20:
		return "\x1b[33m"
	default:
		return "\x1b[31m"
	}
}

func barFilled(left int) int {
	if left <= 0 {
		return 0
	}
	if left >= 100 {
		return 20
	}
	filled := (left*20 + 50) / 100
	if filled == 0 {
		return 1
	}
	if filled == 20 {
		return 19
	}
	return filled
}

func renderBar(left int, useColor bool) string {
	filled := barFilled(left)
	fStr := strings.Repeat("█", filled)
	eStr := strings.Repeat("░", 20-filled)
	if !useColor {
		return fStr + eStr
	}
	var b strings.Builder
	if filled > 0 {
		b.WriteString(colorForLeft(left))
		b.WriteString(fStr)
		b.WriteString("\x1b[0m")
	}
	if filled < 20 {
		b.WriteString("\x1b[90m")
		b.WriteString(eStr)
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func formatRemaining(reset time.Time, resetText string, now time.Time, ja bool) string {
	if reset.IsZero() {
		return resetText
	}
	d := reset.Sub(now)
	totalMins := int(d / time.Minute)
	if d <= 0 || totalMins <= 0 {
		if ja {
			return "まもなく"
		}
		return "soon"
	}
	if d < time.Hour {
		if ja {
			return fmt.Sprintf("あと %d分", totalMins)
		}
		return fmt.Sprintf("in %dm", totalMins)
	}
	if d < 24*time.Hour {
		h := totalMins / 60
		m := totalMins % 60
		if m == 0 {
			if ja {
				return fmt.Sprintf("あと %d時間", h)
			}
			return fmt.Sprintf("in %dh", h)
		}
		if ja {
			return fmt.Sprintf("あと %d時間%d分", h, m)
		}
		return fmt.Sprintf("in %dh %dm", h, m)
	}
	days := totalMins / (24 * 60)
	hours := (totalMins % (24 * 60)) / 60
	if hours == 0 {
		if ja {
			return fmt.Sprintf("あと %d日", days)
		}
		return fmt.Sprintf("in %dd", days)
	}
	if ja {
		return fmt.Sprintf("あと %d日%d時間", days, hours)
	}
	return fmt.Sprintf("in %dd %dh", days, hours)
}

func runeWidth(r rune) int {
	if r < 0x20 || r == 0x7f {
		return 0
	}
	if r < 0x80 {
		return 1
	}
	if unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) ||
		(r >= 0x3000 && r <= 0x303F) ||
		(r >= 0xFF01 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) {
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func padRightWidth(s string, target int) string {
	w := displayWidth(s)
	if w >= target {
		return s
	}
	return s + strings.Repeat(" ", target-w)
}

func truncateWidth(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if displayWidth(s) <= maxW {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > maxW {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String()
}
