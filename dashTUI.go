package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- Message types ---

type tuiTickMsg time.Time
type tuiDoneMsg struct{}

// --- Config ---

type dashConfig struct {
	method       string
	url          string
	workers      int
	rpsLimit     int
	totalReqs    int
	testDuration time.Duration
	durationMode bool
}

// --- Sparkline characters ---

var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// --- Styles ---

var (
	colorAccent  = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7C79FF"}
	colorGood    = lipgloss.AdaptiveColor{Light: "#2E9B3F", Dark: "#5AF78E"}
	colorWarn    = lipgloss.AdaptiveColor{Light: "#C77500", Dark: "#F4BF42"}
	colorError   = lipgloss.AdaptiveColor{Light: "#C41F1F", Dark: "#FF5555"}
	colorSubtle  = lipgloss.AdaptiveColor{Light: "#969B86", Dark: "#5C6370"}
	colorLabel   = lipgloss.AdaptiveColor{Light: "#3C3C3C", Dark: "#ABABAB"}
	colorValue   = lipgloss.AdaptiveColor{Light: "#1A1A2E", Dark: "#F8F8F2"}
	colorSparkLo = lipgloss.AdaptiveColor{Light: "#2E9B3F", Dark: "#5AF78E"}
	colorSparkMd = lipgloss.AdaptiveColor{Light: "#C77500", Dark: "#F4BF42"}
	colorSparkHi = lipgloss.AdaptiveColor{Light: "#C41F1F", Dark: "#FF5555"}
	colorHistBar = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7C79FF"}

	styleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(0, 1)

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent)

	styleURL = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorValue)

	styleSubtitle = lipgloss.NewStyle().
			Foreground(colorSubtle)

	styleDivider = lipgloss.NewStyle().
			Foreground(colorSubtle)

	styleLabel = lipgloss.NewStyle().
			Width(14).
			Foreground(colorLabel)

	styleValue = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorValue)

	styleErrNone = lipgloss.NewStyle().Bold(true).Foreground(colorGood)
	styleErrSome = lipgloss.NewStyle().Bold(true).Foreground(colorError)

	styleBadge2xx = lipgloss.NewStyle().Foreground(colorGood).Bold(true)
	styleBadge4xx = lipgloss.NewStyle().Foreground(colorWarn).Bold(true)
	styleBadge5xx = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	styleBadgeOth = lipgloss.NewStyle().Foreground(colorSubtle)

	styleDone = lipgloss.NewStyle().Bold(true).Foreground(colorGood)

	styleSectionTitle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorAccent)

	styleHistBar  = lipgloss.NewStyle().Foreground(colorHistBar)
	styleHistAxis = lipgloss.NewStyle().Foreground(colorSubtle)

	styleLogOK  = lipgloss.NewStyle().Foreground(colorGood)
	styleLogErr = lipgloss.NewStyle().Foreground(colorError)
	styleLogLat = lipgloss.NewStyle().Foreground(colorValue)
)

// --- Model ---

type dashModel struct {
	cfg   dashConfig
	stats *RunningStats
	prog  progress.Model
	width int
	last  statsSnapshot
	done  bool
}

func newDashModel(cfg dashConfig, rs *RunningStats) dashModel {
	prog := progress.New(
		progress.WithDefaultGradient(),
		progress.WithoutPercentage(),
	)
	return dashModel{
		cfg:   cfg,
		stats: rs,
		prog:  prog,
		width: 70,
	}
}

func doTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return tuiTickMsg(t)
	})
}

func (m dashModel) Init() tea.Cmd {
	return tea.Batch(doTick(), tea.WindowSize())
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		if m.width < 50 {
			m.width = 50
		}
		innerW := m.innerWidth()
		m.prog.Width = innerW - 22
		if m.prog.Width < 15 {
			m.prog.Width = 15
		}
		return m, nil

	case tuiTickMsg:
		if m.done {
			return m, nil
		}
		m.last = m.stats.snapshotFull()

		var fraction float64
		if m.cfg.durationMode {
			fraction = m.last.elapsed.Seconds() / m.cfg.testDuration.Seconds()
		} else if m.cfg.totalReqs > 0 {
			fraction = float64(m.last.completed) / float64(m.cfg.totalReqs)
		}
		if fraction > 1 {
			fraction = 1
		}

		progCmd := m.prog.SetPercent(fraction)
		return m, tea.Batch(doTick(), progCmd)

	case progress.FrameMsg:
		progModel, cmd := m.prog.Update(msg)
		m.prog = progModel.(progress.Model)
		return m, cmd

	case tuiDoneMsg:
		m.done = true
		m.last = m.stats.snapshotFull()
		progCmd := m.prog.SetPercent(1.0)
		return m, tea.Batch(progCmd, tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
			return tea.QuitMsg{}
		}))

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}

	return m, nil
}

func (m dashModel) innerWidth() int {
	w := m.width - 4
	if w > 80 {
		w = 80
	}
	if w < 46 {
		w = 46
	}
	return w
}

func (m dashModel) View() string {
	innerW := m.innerWidth()
	s := m.last
	div := styleDivider.Render(strings.Repeat("─", innerW))

	// ── Header ──
	logo := styleTitle.Render("⚡ RequestReaper")
	urlLine := styleURL.Render(fmt.Sprintf("  %s %s", m.cfg.method, m.cfg.url))

	subtitleParts := []string{fmt.Sprintf("Workers: %d", m.cfg.workers)}
	if m.cfg.rpsLimit > 0 {
		subtitleParts = append(subtitleParts, fmt.Sprintf("RPS: %d", m.cfg.rpsLimit))
	} else {
		subtitleParts = append(subtitleParts, "RPS: unlimited")
	}
	if m.cfg.durationMode {
		subtitleParts = append(subtitleParts, fmt.Sprintf("Duration: %s", m.cfg.testDuration))
	} else {
		subtitleParts = append(subtitleParts, fmt.Sprintf("Requests: %d", m.cfg.totalReqs))
	}
	subtitle := styleSubtitle.Render("  " + strings.Join(subtitleParts, "  |  "))

	// ── Progress ──
	var progressLabel string
	if m.cfg.durationMode {
		elapsed := m.last.elapsed.Truncate(time.Second)
		progressLabel = fmt.Sprintf("%s / %s", elapsed, m.cfg.testDuration)
	} else {
		progressLabel = fmt.Sprintf("%d / %d", s.completed, m.cfg.totalReqs)
	}
	pctStr := fmt.Sprintf("%.0f%%", m.prog.Percent()*100)

	doneLabel := ""
	if m.done {
		doneLabel = styleDone.Render("  Complete!")
	}

	progressRow := fmt.Sprintf("  %s  %s  %s%s",
		m.prog.View(), pctStr, progressLabel, doneLabel)

	// ── Metrics ──
	elapsedStr := fmt.Sprintf("%.1fs", s.elapsed.Seconds())
	rpsStr := fmt.Sprintf("%.1f req/s", s.totalRPS)

	metricLines := []string{
		tuiMetricRow("Elapsed", elapsedStr, "Throughput", rpsStr),
		tuiMetricRow("Avg", fmtMS(s.avg), "P50", fmtMS(s.p50)),
		tuiMetricRow("P95", fmtMS(s.p95), "P99", fmtMS(s.p99)),
		tuiErrRow(s.errCount, s.completed),
	}

	// ── Sparkline ──
	sparkSection := renderSparkline(s.recent, innerW)

	// ── Live histogram ──
	histSection := renderLiveHistogram(s.latencies, innerW)

	// ── Activity log ──
	logSection := renderActivityLog(s.recent, innerW)

	// ── Status codes ──
	statusRow := buildStatusRow(s.statusCodes)

	// ── Compose ──
	sections := []string{
		logo,
		urlLine,
		subtitle,
		div,
		progressRow,
		div,
		strings.Join(metricLines, "\n"),
		div,
		sparkSection,
		div,
		histSection,
	}
	if len(s.recent) > 0 {
		sections = append(sections, div, logSection)
	}
	sections = append(sections, div, statusRow)

	if !m.done {
		hint := styleSubtitle.Render("  Press q to stop")
		sections = append(sections, "", hint)
	}

	body := strings.Join(sections, "\n")
	return "\n" + styleBox.Width(innerW).Render(body) + "\n"
}

// ── Sparkline renderer ──

// renderSparkline renders a time-series sparkline from the most recent requests.
func renderSparkline(recent []recentEntry, width int) string {
	label := styleSectionTitle.Render("  Latency ")

	sparkWidth := width - 16
	if sparkWidth < 10 {
		sparkWidth = 10
	}
	if sparkWidth > 60 {
		sparkWidth = 60
	}

	if len(recent) == 0 {
		return label + styleSubtle.Render("waiting...")
	}

	// Take the most recent sparkWidth entries (time-ordered, oldest first)
	points := recent
	if len(points) > sparkWidth {
		points = points[len(points)-sparkWidth:]
	}

	// Find min/max latency for normalization (skip errors for scale)
	minV, maxV := math.MaxFloat64, 0.0
	for _, e := range points {
		if !e.isError {
			if e.latencyMS < minV {
				minV = e.latencyMS
			}
			if e.latencyMS > maxV {
				maxV = e.latencyMS
			}
		}
	}
	if minV == math.MaxFloat64 {
		minV = 0
	}
	if maxV == minV {
		maxV = minV + 1
	}

	var spark strings.Builder
	for _, e := range points {
		var ch string
		var style lipgloss.Style
		if e.isError {
			ch = "✕"
			style = lipgloss.NewStyle().Foreground(colorError)
		} else {
			normalized := (e.latencyMS - minV) / (maxV - minV)
			idx := int(normalized * float64(len(sparkBlocks)-1))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(sparkBlocks) {
				idx = len(sparkBlocks) - 1
			}
			ch = string(sparkBlocks[idx])
			switch {
			case normalized < 0.4:
				style = lipgloss.NewStyle().Foreground(colorSparkLo)
			case normalized < 0.7:
				style = lipgloss.NewStyle().Foreground(colorSparkMd)
			default:
				style = lipgloss.NewStyle().Foreground(colorSparkHi)
			}
		}
		spark.WriteString(style.Render(ch))
	}

	rangeStr := styleSubtle.Render(fmt.Sprintf(" %.0f-%.0fms", minV, maxV))
	return label + spark.String() + rangeStr
}

// ── Live histogram ──

func renderLiveHistogram(latencies []float64, width int) string {
	title := styleSectionTitle.Render("  Distribution")

	if len(latencies) < 2 {
		return title + "\n" + styleSubtitle.Render("    waiting for data...")
	}

	const numBuckets = 6
	barMaxWidth := width - 28
	if barMaxWidth < 10 {
		barMaxWidth = 10
	}
	if barMaxWidth > 40 {
		barMaxWidth = 40
	}

	minVal := latencies[0]
	maxVal := latencies[len(latencies)-1]
	if maxVal == minVal {
		return title + "\n" + styleSubtitle.Render("    all identical latency")
	}

	bucketWidth := (maxVal - minVal) / float64(numBuckets)
	counts := make([]int, numBuckets)
	for _, v := range latencies {
		idx := int((v - minVal) / bucketWidth)
		if idx >= numBuckets {
			idx = numBuckets - 1
		}
		counts[idx]++
	}

	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}

	var lines []string
	lines = append(lines, title)
	for i := 0; i < numBuckets; i++ {
		lo := minVal + float64(i)*bucketWidth
		hi := lo + bucketWidth
		bw := 0
		if maxCount > 0 {
			bw = int(math.Round(float64(counts[i]) / float64(maxCount) * float64(barMaxWidth)))
		}

		label := styleHistAxis.Render(fmt.Sprintf("  %6.0f-%-6.0f", lo, hi))
		bar := styleHistBar.Render(strings.Repeat("█", bw))
		pad := strings.Repeat(" ", barMaxWidth-bw)
		count := styleSubtle.Render(fmt.Sprintf(" %d", counts[i]))

		lines = append(lines, fmt.Sprintf("%s %s%s%s", label, bar, pad, count))
	}

	return strings.Join(lines, "\n")
}

// ── Activity log ──

func renderActivityLog(recent []recentEntry, width int) string {
	title := styleSectionTitle.Render("  Recent Requests")
	if len(recent) == 0 {
		return title
	}

	var lines []string
	lines = append(lines, title)

	// Show in rows of 3
	perRow := 3
	if width < 60 {
		perRow = 2
	}

	for i := 0; i < len(recent); i += perRow {
		var parts []string
		for j := 0; j < perRow && i+j < len(recent); j++ {
			e := recent[i+j]
			var statusStr string
			if e.isError {
				statusStr = styleLogErr.Render("ERR")
			} else {
				switch {
				case e.status >= 200 && e.status < 300:
					statusStr = styleLogOK.Render(fmt.Sprintf("%d", e.status))
				case e.status >= 400 && e.status < 500:
					statusStr = lipgloss.NewStyle().Foreground(colorWarn).Render(fmt.Sprintf("%d", e.status))
				case e.status >= 500:
					statusStr = styleLogErr.Render(fmt.Sprintf("%d", e.status))
				default:
					statusStr = fmt.Sprintf("%d", e.status)
				}
			}
			latStr := styleLogLat.Render(fmt.Sprintf("%6.1fms", e.latencyMS))
			entry := fmt.Sprintf("  %s %s", statusStr, latStr)
			parts = append(parts, entry)
		}
		lines = append(lines, strings.Join(parts, "    "))
	}

	return strings.Join(lines, "\n")
}

// ── View helpers ──

func tuiMetricRow(labelA, valueA, labelB, valueB string) string {
	left := styleLabel.Render(labelA) + styleValue.Render(valueA)
	right := styleLabel.Render(labelB) + styleValue.Render(valueB)
	return left + "  " + right
}

func tuiErrRow(errCount, total int) string {
	label := styleLabel.Render("Errors")
	errPct := 0.0
	if total > 0 {
		errPct = float64(errCount) / float64(total) * 100
	}
	var val string
	if errCount == 0 {
		val = styleErrNone.Render(fmt.Sprintf("%d (0.0%%)", errCount))
	} else {
		val = styleErrSome.Render(fmt.Sprintf("%d (%.1f%%)", errCount, errPct))
	}
	return label + val
}

func buildStatusRow(codes map[int]int) string {
	if len(codes) == 0 {
		return styleLabel.Render("Status") + styleSubtitle.Render("waiting...")
	}

	keys := make([]int, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	parts := []string{styleLabel.Render("Status")}
	for _, code := range keys {
		badge := fmt.Sprintf("%d: %d", code, codes[code])
		var styled string
		switch {
		case code >= 200 && code < 300:
			styled = styleBadge2xx.Render(badge)
		case code >= 400 && code < 500:
			styled = styleBadge4xx.Render(badge)
		case code >= 500:
			styled = styleBadge5xx.Render(badge)
		default:
			styled = styleBadgeOth.Render(badge)
		}
		parts = append(parts, styled)
	}
	return strings.Join(parts, "  ")
}

func fmtMS(ms float64) string {
	if ms == 0 {
		return "—"
	}
	if ms < 1 {
		return fmt.Sprintf("%.2f ms", ms)
	}
	return fmt.Sprintf("%.1f ms", ms)
}

var styleSubtle = lipgloss.NewStyle().Foreground(colorSubtle)
