package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// wizardResult holds the config gathered from the interactive wizard.
type wizardResult struct {
	url      string
	method   string
	workers  int
	requests int // -1 means duration mode
	duration time.Duration
	timeout  time.Duration
	rps      int
	headers  []string
	body     string
}

// Field indices
const (
	fieldURL = iota
	fieldMethod
	fieldWorkers
	fieldRequests
	fieldDuration
	fieldTimeout
	fieldRPS
	fieldHeaders
	fieldBody
	fieldCount
)

var methodOptions = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD"}

type wizardModel struct {
	inputs   []textinput.Model
	focused  int
	width    int
	err      string
	done     bool
	result   wizardResult
	methodIdx int // index into methodOptions
}

func newWizardModel() wizardModel {
	inputs := make([]textinput.Model, fieldCount)

	inputs[fieldURL] = textinput.New()
	inputs[fieldURL].Placeholder = "https://api.example.com/endpoint"
	inputs[fieldURL].CharLimit = 500
	inputs[fieldURL].Width = 50
	inputs[fieldURL].Focus()

	inputs[fieldMethod] = textinput.New()
	inputs[fieldMethod].Placeholder = "← → to cycle: GET POST PUT DELETE PATCH HEAD"
	inputs[fieldMethod].SetValue("GET")
	inputs[fieldMethod].CharLimit = 10
	inputs[fieldMethod].Width = 50

	inputs[fieldWorkers] = textinput.New()
	inputs[fieldWorkers].Placeholder = "10"
	inputs[fieldWorkers].SetValue("10")
	inputs[fieldWorkers].CharLimit = 6
	inputs[fieldWorkers].Width = 20

	inputs[fieldRequests] = textinput.New()
	inputs[fieldRequests].Placeholder = "100 (leave empty if using duration)"
	inputs[fieldRequests].SetValue("100")
	inputs[fieldRequests].CharLimit = 10
	inputs[fieldRequests].Width = 30

	inputs[fieldDuration] = textinput.New()
	inputs[fieldDuration].Placeholder = "e.g. 30s, 1m (leave empty if using requests)"
	inputs[fieldDuration].CharLimit = 10
	inputs[fieldDuration].Width = 30

	inputs[fieldTimeout] = textinput.New()
	inputs[fieldTimeout].Placeholder = "10s"
	inputs[fieldTimeout].SetValue("10s")
	inputs[fieldTimeout].CharLimit = 10
	inputs[fieldTimeout].Width = 20

	inputs[fieldRPS] = textinput.New()
	inputs[fieldRPS].Placeholder = "0 (unlimited)"
	inputs[fieldRPS].SetValue("0")
	inputs[fieldRPS].CharLimit = 10
	inputs[fieldRPS].Width = 20

	inputs[fieldHeaders] = textinput.New()
	inputs[fieldHeaders].Placeholder = "Key: Value, Key2: Value2"
	inputs[fieldHeaders].CharLimit = 1000
	inputs[fieldHeaders].Width = 50

	inputs[fieldBody] = textinput.New()
	inputs[fieldBody].Placeholder = `{"key": "value"}`
	inputs[fieldBody].CharLimit = 5000
	inputs[fieldBody].Width = 50

	return wizardModel{
		inputs:    inputs,
		focused:   fieldURL,
		width:     80,
		methodIdx: 0,
	}
}

var (
	wizAccent     = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7C79FF"}
	wizSubtle     = lipgloss.AdaptiveColor{Light: "#969B86", Dark: "#5C6370"}
	wizFocused    = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7C79FF"}
	wizError      = lipgloss.AdaptiveColor{Light: "#C41F1F", Dark: "#FF5555"}

	wizTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(wizAccent).
			MarginBottom(1)

	wizLabelStyle = lipgloss.NewStyle().
			Width(14).
			Foreground(lipgloss.AdaptiveColor{Light: "#3C3C3C", Dark: "#ABABAB"})

	wizFocusedLabel = lipgloss.NewStyle().
			Width(14).
			Bold(true).
			Foreground(wizFocused)

	wizBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(wizAccent).
			Padding(1, 2)

	wizHintStyle = lipgloss.NewStyle().
			Foreground(wizSubtle)

	wizErrStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(wizError)

	wizMethodActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#1A1A2E"}).
			Background(wizAccent).
			Padding(0, 1)

	wizMethodInactive = lipgloss.NewStyle().
			Foreground(wizSubtle).
			Padding(0, 1)
)

func (m wizardModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tea.WindowSize())
}

func (m wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.done = true
			return m, tea.Quit

		case "enter":
			if err := m.validate(); err != "" {
				m.err = err
				return m, nil
			}
			m.done = true
			m.result = m.buildResult()
			return m, tea.Quit

		case "tab", "down":
			m.err = ""
			m.inputs[m.focused].Blur()
			m.focused++
			if m.focused >= fieldCount {
				m.focused = 0
			}
			// Skip method field (handled by arrow keys)
			if m.focused == fieldMethod {
				m.focused++
			}
			m.inputs[m.focused].Focus()
			return m, textinput.Blink

		case "shift+tab", "up":
			m.err = ""
			m.inputs[m.focused].Blur()
			m.focused--
			if m.focused < 0 {
				m.focused = fieldCount - 1
			}
			if m.focused == fieldMethod {
				m.focused--
			}
			m.inputs[m.focused].Focus()
			return m, textinput.Blink

		case "left", "right":
			if m.focused == fieldMethod {
				if msg.String() == "right" {
					m.methodIdx = (m.methodIdx + 1) % len(methodOptions)
				} else {
					m.methodIdx = (m.methodIdx - 1 + len(methodOptions)) % len(methodOptions)
				}
				m.inputs[fieldMethod].SetValue(methodOptions[m.methodIdx])
				return m, nil
			}
		}
	}

	// Update focused input
	var cmd tea.Cmd
	m.inputs[m.focused], cmd = m.inputs[m.focused].Update(msg)
	return m, cmd
}

func (m wizardModel) validate() string {
	url := strings.TrimSpace(m.inputs[fieldURL].Value())
	if url == "" {
		return "URL is required"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "URL must start with http:// or https://"
	}

	workersStr := strings.TrimSpace(m.inputs[fieldWorkers].Value())
	if workersStr != "" {
		w, err := strconv.Atoi(workersStr)
		if err != nil || w < 1 {
			return "Workers must be a positive integer"
		}
	}

	reqStr := strings.TrimSpace(m.inputs[fieldRequests].Value())
	durStr := strings.TrimSpace(m.inputs[fieldDuration].Value())

	if reqStr != "" && durStr != "" {
		return "Set either Requests or Duration, not both"
	}
	if reqStr == "" && durStr == "" {
		return "Set either Requests or Duration"
	}

	if reqStr != "" {
		n, err := strconv.Atoi(reqStr)
		if err != nil || n < 1 {
			return "Requests must be a positive integer"
		}
	}
	if durStr != "" {
		if _, err := time.ParseDuration(durStr); err != nil {
			return "Duration must be valid (e.g. 30s, 1m, 2m30s)"
		}
	}

	timeoutStr := strings.TrimSpace(m.inputs[fieldTimeout].Value())
	if timeoutStr != "" {
		if _, err := time.ParseDuration(timeoutStr); err != nil {
			return "Timeout must be valid (e.g. 10s, 30s)"
		}
	}

	rpsStr := strings.TrimSpace(m.inputs[fieldRPS].Value())
	if rpsStr != "" {
		r, err := strconv.Atoi(rpsStr)
		if err != nil || r < 0 {
			return "RPS must be a non-negative integer"
		}
	}

	return ""
}

func (m wizardModel) buildResult() wizardResult {
	r := wizardResult{
		url:     strings.TrimSpace(m.inputs[fieldURL].Value()),
		method:  methodOptions[m.methodIdx],
		workers: 10,
		timeout: 10 * time.Second,
	}

	if v := strings.TrimSpace(m.inputs[fieldWorkers].Value()); v != "" {
		r.workers, _ = strconv.Atoi(v)
	}

	reqStr := strings.TrimSpace(m.inputs[fieldRequests].Value())
	durStr := strings.TrimSpace(m.inputs[fieldDuration].Value())

	if durStr != "" {
		r.requests = -1
		r.duration, _ = time.ParseDuration(durStr)
	} else {
		r.requests, _ = strconv.Atoi(reqStr)
	}

	if v := strings.TrimSpace(m.inputs[fieldTimeout].Value()); v != "" {
		r.timeout, _ = time.ParseDuration(v)
	}

	if v := strings.TrimSpace(m.inputs[fieldRPS].Value()); v != "" {
		r.rps, _ = strconv.Atoi(v)
	}

	if v := strings.TrimSpace(m.inputs[fieldHeaders].Value()); v != "" {
		for _, h := range strings.Split(v, ",") {
			h = strings.TrimSpace(h)
			if h != "" {
				r.headers = append(r.headers, h)
			}
		}
	}

	r.body = strings.TrimSpace(m.inputs[fieldBody].Value())
	return r
}

func (m wizardModel) View() string {
	innerW := m.width - 8
	if innerW > 76 {
		innerW = 76
	}
	if innerW < 50 {
		innerW = 50
	}

	title := wizTitleStyle.Render("⚡ RequestReaper — Configuration")

	fields := []struct {
		label string
		idx   int
	}{
		{"URL", fieldURL},
		{"Method", fieldMethod},
		{"Workers", fieldWorkers},
		{"Requests", fieldRequests},
		{"Duration", fieldDuration},
		{"Timeout", fieldTimeout},
		{"RPS Limit", fieldRPS},
		{"Headers", fieldHeaders},
		{"Body", fieldBody},
	}

	var lines []string
	lines = append(lines, title)
	lines = append(lines, "")

	for _, f := range fields {
		label := wizLabelStyle.Render(f.label)
		if m.focused == f.idx {
			label = wizFocusedLabel.Render(f.label)
		}

		if f.idx == fieldMethod {
			// Render method selector
			var methods []string
			for i, opt := range methodOptions {
				if i == m.methodIdx {
					methods = append(methods, wizMethodActive.Render(opt))
				} else {
					methods = append(methods, wizMethodInactive.Render(opt))
				}
			}
			hint := ""
			if m.focused == fieldMethod || m.focused == fieldURL {
				hint = wizHintStyle.Render("  (Tab to skip, ← → to cycle)")
			}
			lines = append(lines, label+strings.Join(methods, " ")+hint)
		} else {
			lines = append(lines, label+m.inputs[f.idx].View())
		}
	}

	lines = append(lines, "")

	if m.err != "" {
		lines = append(lines, wizErrStyle.Render("  ✗ "+m.err))
		lines = append(lines, "")
	}

	hints := []string{
		wizHintStyle.Render("  Tab/Shift+Tab: navigate"),
		wizHintStyle.Render("  Enter: start test"),
		wizHintStyle.Render("  Ctrl+C: quit"),
	}
	lines = append(lines, strings.Join(hints, "    "))

	body := strings.Join(lines, "\n")
	return "\n" + wizBoxStyle.Width(innerW).Render(body) + "\n"
}

// runWizard launches the interactive wizard and returns the result.
// Returns (result, true) on success, or (zero, false) if user cancelled.
func runWizard() (wizardResult, bool) {
	m := newWizardModel()
	p := tea.NewProgram(m, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return wizardResult{}, false
	}

	wm := finalModel.(wizardModel)
	if wm.result.url == "" {
		return wizardResult{}, false
	}
	return wm.result, true
}
