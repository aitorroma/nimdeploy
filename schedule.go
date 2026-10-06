package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

const TriggerSchedule = "schedule"

// cronSpec is a parsed schedule: five cron fields (minute hour day-of-month
// month day-of-week, in the server's local time) or "@every <duration>".
type cronSpec struct {
	every                         time.Duration
	minute, hour, dom, month, dow []bool
	domAny, dowAny                bool
}

var cronAliases = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var cronNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

func parseCron(s string) (*cronSpec, error) {
	s = strings.TrimSpace(s)
	if d, ok := strings.CutPrefix(s, "@every "); ok {
		dur, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil || dur < time.Second {
			return nil, fmt.Errorf("@every needs a duration of at least 1s, like @every 10m")
		}
		return &cronSpec{every: dur}, nil
	}
	if alias, ok := cronAliases[s]; ok {
		s = alias
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return nil, fmt.Errorf("%q: want 5 fields (minute hour day month weekday), @hourly/@daily/@weekly/@monthly or @every 10m", s)
	}
	c := &cronSpec{}
	var err error
	if c.minute, err = cronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if c.hour, err = cronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if c.dom, err = cronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	if c.month, err = cronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if c.dow, err = cronField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	c.dow[0] = c.dow[0] || c.dow[7] // 7 is Sunday too
	c.domAny, c.dowAny = fields[2] == "*", fields[4] == "*"
	return c, nil
}

// cronField parses "*", "5", "1-5", "*/15", "0-30/10", "mon-fri", "1,15".
func cronField(f string, lo, hi int) ([]bool, error) {
	set := make([]bool, hi+1)
	for _, part := range strings.Split(strings.ToLower(f), ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("bad step in %q", part)
			}
			step = n
		}
		from, to := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if from, err = cronValue(a, lo, hi); err != nil {
				return nil, err
			}
			to = from
			if isRange {
				if to, err = cronValue(b, lo, hi); err != nil {
					return nil, err
				}
			} else if hasStep {
				to = hi
			}
			if to < from {
				return nil, fmt.Errorf("range %q goes backwards", rng)
			}
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return set, nil
}

func cronValue(s string, lo, hi int) (int, error) {
	if n, ok := cronNames[s]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%q is not between %d and %d", s, lo, hi)
	}
	return n, nil
}

// next returns the first run strictly after t.
func (c *cronSpec) next(t time.Time) time.Time {
	if c.every > 0 {
		return t.Add(c.every)
	}
	t = t.Truncate(time.Minute).Add(time.Minute)
	for limit := t.AddDate(5, 0, 0); t.Before(limit); {
		if !c.month[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !c.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{} // e.g. "0 0 31 2 *": never
}

// dayMatches follows cron: with both day fields restricted, either matches.
func (c *cronSpec) dayMatches(t time.Time) bool {
	dom, dow := c.dom[t.Day()], c.dow[int(t.Weekday())]
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// scheduler starts scheduled deploys at their time, until the runner stops.
// It re-reads the config on every wake-up, so reloads apply.
func (r *Runner) scheduler() {
	defer close(r.schedDone)
	type entry struct {
		spec *cronSpec
		at   time.Time
	}
	next := map[string]entry{}
	for {
		r.mu.Lock()
		cfg := r.cfg
		r.mu.Unlock()

		now := time.Now()
		wake := now.Add(time.Minute)
		for _, name := range cfg.DeployNames() {
			d := cfg.Deploy[name]
			if d.schedule == nil {
				delete(next, name)
				continue
			}
			e, ok := next[name]
			if !ok || e.spec != d.schedule { // new, or changed by a reload
				e = entry{spec: d.schedule, at: d.schedule.next(now)}
				next[name] = e
				r.setNextRun(name, e.at)
			}
			at := e.at
			if at.IsZero() {
				continue
			}
			if !now.Before(at) {
				res, err := r.Submit(name, Trigger{Source: TriggerSchedule, Provider: d.Provider,
					Repository: d.Repository, Branch: d.Branch, Pusher: "schedule"})
				switch {
				case err != nil:
					log.Printf("deploy=%s scheduled run not started: %v", name, err)
				case res.Result == ResultQueued:
					log.Printf("deploy=%s scheduled run queued behind the running one", name)
				}
				at = d.schedule.next(now)
				next[name] = entry{spec: d.schedule, at: at}
				r.setNextRun(name, at)
			}
			if !at.IsZero() && at.Before(wake) {
				wake = at
			}
		}
		for name := range next {
			if d, ok := cfg.Deploy[name]; !ok || d.schedule == nil {
				delete(next, name)
				r.setNextRun(name, time.Time{})
			}
		}
		select {
		case <-r.stopSched:
			return
		case <-r.reload:
		case <-time.After(time.Until(wake)):
		}
	}
}

// setNextRun publishes the next scheduled run on /status.
func (r *Runner) setNextRun(name string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at.IsZero() {
		delete(r.nextRun, name)
		return
	}
	r.nextRun[name] = at.Truncate(time.Second)
}
