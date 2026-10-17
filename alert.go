package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// downAfter is how many failed checks in a row page (owner, 2026-10-13).
const downAfter = 3

// alertState is one endpoint's run of failures and whether it has paged (or the checker's own
// run of no-data checks).
type alertState struct {
	Fails   int       `json:"fails"`          // failed checks in a row (no-data minutes neither count nor break the run)
	Since   time.Time `json:"since,omitzero"` // the run's first failed check
	Pushed  bool      `json:"pushed"`         // the down push went out
	LastErr string    `json:"last_err,omitempty"`
}

// event is one endpoint's state change worth a push.
type event struct {
	Endpoint string
	Down     bool   // went down (else came back)
	Detail   string // "since 14:06 UTC (3 checks in a row): HTTP 502", "down 8m (since 14:06 UTC)"
}

// step moves one endpoint's alert state on by one result. It returns the event to push, if any:
// down on the downAfter'th failure in a row (and every failure after it until one is
// delivered: the caller sets Pushed only once the push went out), up on the first good check
// after a delivered down push. Nothing else pages.
func step(s alertState, endpoint string, r Result, at time.Time) (alertState, *event) {
	switch r.State {
	case Failed:
		if s.Fails == 0 {
			s.Since = at
		}
		s.Fails++
		s.LastErr = r.Err
		if s.Fails >= downAfter && !s.Pushed {
			return s, &event{Endpoint: endpoint, Down: true,
				Detail: fmt.Sprintf("since %s UTC (%d checks in a row): %s", hhmm(s.Since), s.Fails, r.Err)}
		}
		return s, nil
	case Good:
		if s.Pushed {
			return alertState{}, &event{Endpoint: endpoint,
				Detail: fmt.Sprintf("down %s (since %s UTC)", minutes(at.Sub(s.Since)), hhmm(s.Since))}
		}
		return alertState{}, nil
	}
	return s, nil // no data: the checker's own network, not boot.watch
}

func hhmm(t time.Time) string { return t.UTC().Format("15:04") }

// minutes is a duration to the minute: "8m", "1h25m".
func minutes(d time.Duration) string {
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// blindAfter is how many no-data checks in a row make the checker push that it can't check
// (owner, 2026-10-17): its control has failed for 10 minutes, and the page only says "Status
// unknown".
const blindAfter = 10

// blindStep moves the checker's own state on by one check. No data counts like a failure: the
// blindAfter'th in a row pushes (again every check until one is delivered: the caller sets
// Pushed); the first check with data after a delivered push pushes that it's checking again,
// and after an undelivered one (the network itself was down) that it was blind, and when.
func blindStep(s alertState, r Result, at time.Time) (alertState, *message) {
	if r.State == NoData {
		if s.Fails == 0 {
			s.Since = at
		}
		s.Fails++
		s.LastErr = r.Err
		if s.Fails >= blindAfter && !s.Pushed {
			return s, &message{Down: true, Title: "bootwatcherd can't check boot.watch",
				Body: fmt.Sprintf("no data since %s UTC (%d checks in a row): %s", hhmm(s.Since), s.Fails, s.LastErr)}
		}
		return s, nil
	}
	switch {
	case s.Pushed:
		return alertState{}, &message{Title: "bootwatcherd checking again",
			Body: fmt.Sprintf("blind %s (since %s UTC)", minutes(at.Sub(s.Since)), hhmm(s.Since))}
	case s.Fails >= blindAfter:
		return alertState{}, &message{Title: "bootwatcherd was blind",
			Body: fmt.Sprintf("%s–%s UTC (%s): %s", hhmm(s.Since), hhmm(at), minutes(at.Sub(s.Since)), s.LastErr)}
	}
	return alertState{}, nil
}

// message is a push.
type message struct {
	Title, Body string
	Down        bool
	Endpoints   []string // the endpoints it reports
}

// combine turns one check's events into one push per state change (owner, 2026-10-14): at most
// one for what went down and one for what came back, however many endpoints changed.
func combine(evs []event) []message {
	var out []message
	for _, down := range []bool{true, false} {
		var paths, details []string
		for _, e := range evs {
			if e.Down == down {
				paths = append(paths, e.Endpoint)
				details = append(details, e.Detail)
			}
		}
		if len(paths) == 0 {
			continue
		}
		who := "boot.watch " + strings.Join(paths, ", ")
		if len(paths) == len(endpoints) {
			who = "boot.watch"
		}
		m := message{Down: down, Endpoints: paths}
		switch {
		case down && who == "boot.watch":
			m.Title = "boot.watch not answering"
		case down:
			m.Title = who + " failing"
		default:
			m.Title = who + " answering again"
		}
		same := true
		for _, d := range details {
			same = same && d == details[0]
		}
		if same {
			m.Body = details[0]
			if len(paths) > 1 && len(paths) < len(endpoints) {
				m.Body = strings.Join(paths, ", ") + ": " + details[0]
			}
		} else {
			lines := make([]string, len(paths))
			for i := range paths {
				lines[i] = paths[i] + ": " + details[i]
			}
			m.Body = strings.Join(lines, "\n")
		}
		out = append(out, m)
	}
	return out
}

// pushover sends pushes through the Pushover API.
type pushover struct {
	token, user string
	api         string // https://api.pushover.net/1/messages.json
	page        string // the link in every push
	hc          *http.Client
}

// loadPushover reads PUSHOVER_TOKEN= and PUSHOVER_USER= lines from path.
func loadPushover(path, page string) (*pushover, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := &pushover{api: "https://api.pushover.net/1/messages.json", page: page, hc: &http.Client{Timeout: 10 * time.Second}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		switch k {
		case "PUSHOVER_TOKEN":
			p.token = v
		case "PUSHOVER_USER":
			p.user = v
		}
	}
	if p.token == "" || p.user == "" {
		return nil, fmt.Errorf("%s: PUSHOVER_TOKEN and PUSHOVER_USER needed", path)
	}
	return p, sc.Err()
}

// testPush sends one push to show that pushes arrive (-test-push; every deploy runs it).
func testPush(ctx context.Context, path, page string) error {
	p, err := loadPushover(path, page)
	if err != nil {
		return err
	}
	return p.send(ctx, message{Title: "bootwatcherd test push", Body: "bootwatcherd " + version + " reaches you through Pushover."})
}

func (p *pushover) send(ctx context.Context, m message) error {
	// Pushover turns away titles over 250 characters and messages over 1024
	form := url.Values{"token": {p.token}, "user": {p.user}, "title": {cut(m.Title, 250)}, "message": {cut(m.Body, 1000)},
		"url": {p.page}, "url_title": {"status.boot.watch"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.api, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.hc.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err // the reason, without the URL
		}
		return err
	}
	defer res.Body.Close()
	var a struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&a)
	if res.StatusCode != http.StatusOK || a.Status != 1 {
		return fmt.Errorf("pushover: HTTP %d %s", res.StatusCode, strings.Join(a.Errors, "; "))
	}
	return nil
}
