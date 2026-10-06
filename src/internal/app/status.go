package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
)

// srvStatus is the state of the connection to the RomM server, shown in the status bar.
type srvStatus struct {
	state   string // checking | connected | not_paired | unauthorized | unreachable | not_configured
	version string
	err     string
}

// checkServer asks the server for its heartbeat and, if this device holds a token, checks that
// the token is accepted. It runs in the background; the result arrives as a srvStatus message.
func (m *model) checkServer() tea.Cmd {
	base := m.cfg("server_url", "")
	if base == "" {
		m.srv = srvStatus{state: "not_configured"}
		return nil
	}
	m.srv = srvStatus{state: "checking"}
	c, token := m.client(), m.data.Token
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		hreq, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/heartbeat", nil)
		resp, err := c.Do(hreq)
		if err != nil {
			return srvStatus{state: "unreachable", err: err.Error()}
		}
		var hb struct {
			System struct {
				Version string `json:"VERSION"`
			} `json:"SYSTEM"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(&hb)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return srvStatus{state: "unreachable", err: "HTTP " + strconv.Itoa(resp.StatusCode)}
		}
		st := srvStatus{state: "not_paired", version: hb.System.Version}
		if token == "" {
			return st
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/platforms", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r2, err := c.Do(req)
		if err != nil {
			return srvStatus{state: "unreachable", version: st.version, err: err.Error()}
		}
		r2.Body.Close()
		switch {
		case r2.StatusCode == 200:
			st.state = "connected"
		case r2.StatusCode == 401 || r2.StatusCode == 403:
			st.state = "unauthorized"
		default:
			st.state, st.err = "unreachable", "HTTP "+strconv.Itoa(r2.StatusCode)
		}
		return st
	}
}

// srvBadge is the colored label of the server state.
func (m *model) srvBadge(room int) string {
	v := ""
	if m.srv.version != "" {
		v = " RomM " + m.srv.version
	}
	var label string
	var s = m.curStyle(kMessage) // green
	switch m.srv.state {
	case "connected":
		label = " connected" + v + " "
	case "not_paired":
		label, s = " not paired"+v+" ", m.curStyle(kQuestion)
	case "unauthorized":
		label, s = " token rejected"+v+" ", m.sBad
	case "unreachable":
		label, s = " unreachable ", m.sBad
	case "not_configured":
		label, s = " no server set ", m.curStyle(kQuestion)
	case "clock":
		label, s = " setting the clock... ", m.curStyle(kQuestion)
	default:
		label, s = " checking... ", m.sDim
	}
	if room > 0 && len([]rune(label)) > room { // narrow terminal: drop the version
		label = map[string]string{"connected": " connected ", "not_paired": " not paired ", "unauthorized": " token rejected "}[m.srv.state]
		if label == "" {
			label = " " + m.srv.state + " "
		}
	}
	return s.Render(label)
}
