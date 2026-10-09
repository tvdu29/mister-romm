package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
)

// onJob handles what the background jobs report.
func (m *model) onJob(f fromJob) tea.Cmd {
	switch x := f.msg.(type) {
	case syncProgress:
		m.syncText = x.text
	case syncDone:
		return m.finishSync(x)
	case dlStart, dlProgress, dlDoneMsg:
		m.onDownloadMsg(x)
	case toolLine:
		if m.pop != nil && m.pop.kind == "tool" {
			m.pop.lines = append(m.pop.lines, x.text)
		}
	case clockResult:
		return m.finishClock(x)
	case jobClosed:
		switch f.j {
		case m.syncJob:
			m.syncJob, m.syncText = nil, ""
		case m.dlJob:
			m.dlJob = nil
			m.downloadSummary()
			return m.kickDownloads()
		case m.toolJob:
			m.toolJob = nil
			if m.pop != nil && m.pop.kind == "tool" {
				m.pop.running = false
			}
		case m.clockJob:
			m.clockJob = nil
		}
	}
	return nil
}

func (m *model) finishClock(r clockResult) tea.Cmd {
	if r.ok {
		return tea.Batch(m.checkServer(), m.startUpdateCheck())
	}
	m.srv = srvStatus{state: "clock"}
	m.confirm("Clock not set", []string{
		"The clock of this device is not set (" + dateNow() + "),",
		"so HTTPS certificates look \"not yet valid\".",
		"Network time could not be obtained (network not ready yet?).",
		"",
		"Continue WITHOUT certificate verification, for this session only?"}, func(m *model) tea.Cmd {
		m.sessionInsecure = true
		return m.checkServer()
	})
	return nil
}

func dateNow() string { return time.Now().Format("2006-01-02") }

// openDoctor runs the diagnostics and shows the report as it is produced.
func (m *model) openDoctor() tea.Cmd {
	if m.toolJob != nil {
		m.toast = "Another test is already running"
		return nil
	}
	e := m.env()
	in := docInfo{version: version, term: os.Getenv("TERM"), w: m.w, h: m.h,
		stdinTerminal: isTerminal(os.Stdin), lastErrors: tailLines(filepath.Join(m.cfgDir(), "last-error.log"), 8),
		selected: len(readSelection(m.cfgDir())), games: len(m.data.Games),
		configured: m.cfg("server_url", "") != "", paired: m.data.Token != ""}
	m.pop = &popup{kind: "tool", title: "Diagnostics", running: true}
	j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) { runDoctor(ctx, e, in, send) })
	m.toolJob = j
	return cmd
}

func (m *model) openSpeedTest() tea.Cmd {
	if m.toolJob != nil {
		m.toast = "Another test is already running"
		return nil
	}
	m.confirm("Speed test", []string{
		"Measures the internet link (downloads 20 MB from speed.cloudflare.com),",
		"the card write speed (32 MB, deleted) and your RomM server (the start of",
		"one game, up to 12 s). Nothing is saved. It takes about 40 seconds.", "",
		"Run it?"}, func(m *model) tea.Cmd {
		e := m.env()
		m.pop = &popup{kind: "tool", title: "Speed test", running: true}
		j, cmd := startJob(func(ctx context.Context, send func(tea.Msg)) { runSpeed(ctx, e, send) })
		m.toolJob = j
		return cmd
	})
	return nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
