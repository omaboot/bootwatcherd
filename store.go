package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// store is bootwatcherd's own files (never doiomad's), plain text in its state directory:
//
//	checks/2026-10-17.tsv  a UTC day's checks, one line per endpoint per check:
//	                       at (unix seconds), endpoint, state (0 no data, 1 good, 2 failed),
//	                       round trip (ms), address family (4, 6 or empty), why it failed
//	days.tsv               the summaries of days whose checks have gone (after 60 days):
//	                       date, endpoint, good, failed, median ms, 95th percentile ms
//	state.json             the alert states and bootwatchd's restarts
//
// The last 60 days live in memory too (about 1 MB), and the page reads only memory.
type store struct {
	dir string

	mu     sync.Mutex
	days   map[string]map[string]*dayAcc // UTC date → endpoint path → its checks
	last   *Check
	health *Health
	state  persisted
	saved  []byte // state.json as last written
	gen    uint64 // counts the changes the page shows
	f      *os.File
	fDay   string // the day f appends to
}

// dayAcc is one endpoint's checks on one UTC day.
type dayAcc struct {
	good, failed int
	rtts         []int32 // the good checks' round trips, ms
	sum          *Day    // the summary, until the next check comes
}

// persisted is state.json.
type persisted struct {
	Alerts      map[string]alertState `json:"alerts"`
	Blind       alertState            `json:"blind"`                 // the checker's own run of no-data checks
	Bugs        bugAlert              `json:"bugs"`                  // bootwatchd's bug reports, as last seen
	WatchdSince time.Time             `json:"watchd_since,omitzero"` // bootwatchd's start, as last seen
	Restarts    []time.Time           `json:"restarts,omitempty"`    // bootwatchd's restarts in the last 60 days
}

var dayFile = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.tsv$`)

// openStore reads (creating) the store in dir, then drops what's older than the days shown at now.
func openStore(dir string, now time.Time) (*store, error) {
	s := &store{dir: dir, days: map[string]map[string]*dayAcc{}, state: persisted{Alerts: map[string]alertState{}}}
	if err := os.MkdirAll(filepath.Join(dir, "checks"), 0o700); err != nil {
		return nil, err
	}
	switch b, err := os.ReadFile(filepath.Join(dir, "state.json")); {
	case err == nil:
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("state.json: %w", err)
		}
		if s.state.Alerts == nil {
			s.state.Alerts = map[string]alertState{}
		}
		s.saved = b
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	// a write cut short by a crash leaves its temporary file
	tmps, _ := filepath.Glob(filepath.Join(dir, "state-*.tmp"))
	for _, t := range tmps {
		os.Remove(t)
	}
	files, err := os.ReadDir(filepath.Join(dir, "checks"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if dayFile.MatchString(f.Name()) {
			if err := s.load(f.Name()); err != nil {
				return nil, err
			}
		}
	}
	return s, s.prune(now)
}

// load reads one day's file. A line it can't read (one a crash cut short) is skipped.
func (s *store) load(name string) error {
	f, err := os.Open(filepath.Join(s.dir, "checks", name))
	if err != nil {
		return err
	}
	defer f.Close()
	bad := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		at, path, r, ok := parseLine(sc.Text())
		if !ok {
			bad++
			continue
		}
		if !known(path) {
			continue
		}
		s.add(time.Unix(at, 0).UTC(), path, r)
		switch {
		case s.last == nil || at > s.last.At.Unix():
			s.last = &Check{At: time.Unix(at, 0).UTC(), Results: map[string]Result{path: r}}
		case at == s.last.At.Unix():
			s.last.Results[path] = r
		}
	}
	if bad > 0 {
		log.Printf("bootwatcherd: checks/%s: skipped %d unreadable lines", name, bad)
	}
	return sc.Err()
}

func parseLine(line string) (at int64, path string, r Result, ok bool) {
	f := strings.Split(line, "\t")
	if len(f) != 6 {
		return 0, "", r, false
	}
	at, err1 := strconv.ParseInt(f[0], 10, 64)
	st, err2 := strconv.Atoi(f[2])
	ms, err3 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || st < int(NoData) || st > int(Failed) {
		return 0, "", r, false
	}
	return at, f[1], Result{State: State(st), RTT: time.Duration(ms) * time.Millisecond, Family: f[4], Err: f[5]}, true
}

func known(path string) bool {
	return slices.ContainsFunc(endpoints, func(e Endpoint) bool { return e.Path == path })
}

// add counts one result into its day.
func (s *store) add(at time.Time, path string, r Result) {
	d := dayOf(at)
	if s.days[d] == nil {
		s.days[d] = map[string]*dayAcc{}
	}
	a := s.days[d][path]
	if a == nil {
		a = &dayAcc{}
		s.days[d][path] = a
	}
	switch r.State {
	case Good:
		a.good++
		a.rtts = append(a.rtts, int32(min(r.RTT.Milliseconds(), 1<<31-1)))
	case Failed:
		a.failed++
	}
	a.sum = nil
}

func (s *store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

func dayOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// save records a check, notes a bootwatchd restart, and appends the check to its day's file
// in one write.
func (s *store) save(c Check) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := c.At.UTC()
	last := &Check{At: at, Results: map[string]Result{}, Health: c.Health}
	var b strings.Builder
	for _, e := range endpoints {
		r := c.Results[e.Path]
		r.Err = clip(r.Err, maxErr)
		s.add(at, e.Path, r)
		last.Results[e.Path] = r
		fmt.Fprintf(&b, "%d\t%s\t%d\t%d\t%s\t%s\n", at.Unix(), e.Path, r.State, r.RTT.Milliseconds(), r.Family, r.Err)
	}
	s.last = last
	var perr error
	if h := c.Health; h != nil {
		s.health = h
		if !h.Since.IsZero() && !h.Since.Equal(s.state.WatchdSince) {
			if !s.state.WatchdSince.IsZero() {
				s.state.Restarts = append(s.state.Restarts, h.Since.UTC())
			}
			s.state.WatchdSince = h.Since.UTC()
			perr = s.persist()
		}
	}
	s.gen++
	return errors.Join(s.append(dayOf(at), b.String()), perr)
}

// append writes lines to day's file, opening it when the day changes.
func (s *store) append(day, lines string) error {
	if s.f == nil || s.fDay != day {
		if s.f != nil {
			s.f.Close()
			s.f = nil
		}
		f, err := os.OpenFile(filepath.Join(s.dir, "checks", day+".tsv"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		// a line a crash cut short gets its end, so the next line starts on its own
		if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
			end := make([]byte, 1)
			if _, err := f.ReadAt(end, fi.Size()-1); err == nil && end[0] != '\n' {
				f.WriteString("\n")
			}
		}
		s.f, s.fDay = f, day
	}
	_, err := io.WriteString(s.f, lines)
	return err
}

// summary is the day's Day for path; zero counts when it has no checks.
func (s *store) summary(date, path string) Day {
	a := s.days[date][path]
	if a == nil {
		return Day{Date: date}
	}
	if a.sum == nil {
		d := Day{Date: date, Good: a.good, Failed: a.failed}
		if n := len(a.rtts); n > 0 {
			r := slices.Clone(a.rtts)
			slices.Sort(r)
			d.MedianMS = int(r[n/2])
			d.P95MS = int(r[(95*n+99)/100-1]) // nearest rank
		}
		a.sum = &d
	}
	return *a.sum
}

// prune moves the days before the 60 shown at now out of memory and into days.tsv, deletes
// their files, and forgets bootwatchd's restarts from before them.
func (s *store) prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := dayOf(now.UTC().AddDate(0, 0, -(days - 1)))
	var old []string
	for d := range s.days {
		if d < first {
			old = append(old, d)
		}
	}
	files, err := os.ReadDir(filepath.Join(s.dir, "checks"))
	if err != nil {
		return err
	}
	for _, f := range files {
		if d := strings.TrimSuffix(f.Name(), ".tsv"); dayFile.MatchString(f.Name()) && d < first && s.days[d] == nil {
			old = append(old, d) // a day with no checks left in it
		}
	}
	slices.Sort(old)
	var b strings.Builder
	for _, d := range old {
		for _, e := range endpoints {
			if s.days[d][e.Path] != nil {
				sum := s.summary(d, e.Path)
				fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\t%d\n", d, e.Path, sum.Good, sum.Failed, sum.MedianMS, sum.P95MS)
			}
		}
	}
	if b.Len() > 0 {
		f, err := os.OpenFile(filepath.Join(s.dir, "days.tsv"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, b.String())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err // the files stay until their summaries are written
		}
	}
	for _, d := range old {
		delete(s.days, d)
		if err := os.Remove(filepath.Join(s.dir, "checks", d+".tsv")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	n := len(s.state.Restarts)
	s.state.Restarts = slices.DeleteFunc(s.state.Restarts, func(t time.Time) bool { return dayOf(t) < first })
	if len(old) > 0 || len(s.state.Restarts) != n {
		s.gen++
	}
	return s.persist()
}

// view is the page's data at now, from memory.
func (s *store) view(now time.Time) View {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := View{Now: now}
	first := now.UTC().AddDate(0, 0, -(days - 1))
	for _, e := range endpoints {
		row := Row{Endpoint: e, Days: make([]Day, days), Uptime: -1}
		var good, failed int
		for i := range row.Days {
			d := s.summary(dayOf(first.AddDate(0, 0, i)), e.Path)
			row.Days[i] = d
			good += d.Good
			failed += d.Failed
		}
		if good+failed > 0 {
			row.Uptime = 100 * float64(good) / float64(good+failed)
		}
		v.Rows = append(v.Rows, row)
	}
	if s.last != nil {
		c := *s.last
		c.Results = maps.Clone(s.last.Results)
		v.Last = &c
	}
	if s.health != nil {
		h := *s.health
		v.Health = &h
	}
	for _, t := range s.state.Restarts {
		if dayOf(t) >= dayOf(first) {
			v.Restarts = append(v.Restarts, t)
		}
	}
	return v
}

// lastAt is the newest check's time; zero with none.
func (s *store) lastAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return time.Time{}
	}
	return s.last.At
}

// generation changes whenever the page's data does.
func (s *store) generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// alertStates are every endpoint's alert state and the checker's own.
func (s *store) alertStates() (map[string]alertState, alertState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]alertState{}
	for _, e := range endpoints {
		m[e.Path] = s.state.Alerts[e.Path]
	}
	return m, s.state.Blind
}

// bugAlert is the bug report alerts' state.
func (s *store) bugAlert() bugAlert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Bugs
}

// setBugAlert keeps the bug report alerts' state, writing state.json when it changed.
func (s *store) setBugAlert(b bugAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Bugs = b
	return s.persist()
}

// setAlertStates keeps the alert states, writing state.json when they changed.
func (s *store) setAlertStates(m map[string]alertState, blind alertState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, a := range m {
		s.state.Alerts[k] = a
	}
	s.state.Blind = blind
	return s.persist()
}

// persist writes state.json when it changed: a new file renamed over the old, so a crash leaves
// one or the other whole.
func (s *store) persist() error {
	b, err := json.MarshalIndent(s.state, "", "\t")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if bytes.Equal(b, s.saved) {
		return nil
	}
	f, err := os.CreateTemp(s.dir, "state-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "state.json"))
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	s.saved = b
	return nil
}
