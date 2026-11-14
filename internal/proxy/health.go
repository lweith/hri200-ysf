package proxy

import (
	"fmt"
	"time"

	"github.com/lweith/hri200-ysf/internal/hri"
)

// The master's watch on the radio. A radio that stops answering, sits in a
// transmit state nobody keyed, or fails transmission after transmission is
// sick: no transmissions are sent it while it is, and the master works to get
// it back: unkey polls and a status query first, then WIRES-X's startup over
// again, then (with Master.Recover) a reset of its USB device, then a cycle
// of its port's power. WIRES-X sees none of it; the face carries on.

const (
	silentAfter  = 5 * time.Second // unanswered this long: not answering
	stuckAfter   = 5 * time.Second // in a transmit state nobody keyed
	faultWindow  = 2 * time.Minute // this many faults within it make it sick
	faultsToSick = 3
	nudgeEvery   = 2 * time.Second // unkey polls, this often
	nudges       = 3
	reinitWait   = 10 * time.Second // after re-running the startup, before a reset
	recoverGap   = 30 * time.Second // between resets
	recoverRest  = 5 * time.Minute  // after the last level, before trying again
	wellAfter    = 10 * time.Second // healthy this long: well again
)

type health struct {
	sick       bool
	why        string
	heardAt    time.Time
	unanswered int
	stuck      time.Time // since when in a transmit state nobody keyed
	faults     []time.Time
	wellSince  time.Time
	nudged     int
	reinited   bool      // WIRES-X's startup re-run
	actAt      time.Time // the last recovery step
}

// Ladder is how far recovery has gone, kept across the HRI-200 going away
// and coming back (a reset makes it do that).
type Ladder struct {
	Level int // the next recovery level: 1 USB reset, 2 port power
	At    time.Time
}

func (h *health) reset(now time.Time) {
	*h = health{heardAt: now}
}

func (h *health) sent(time.Time)      { h.unanswered++ }
func (h *health) heard(now time.Time) { h.heardAt, h.unanswered = now, 0 }

// state notes a state; ours says whether the master has the radio keyed.
func (h *health) state(s hri.State, now time.Time, ours bool) {
	if !ours && len(s) == 5 && s[1] == '0' {
		if h.stuck.IsZero() {
			h.stuck = now
		}
		return
	}
	h.stuck = time.Time{}
}

// fault notes a transmission the radio failed.
func (m *Master) fault(now time.Time, why string) {
	m.logf("master: RADIO FAULT: %s", why)
	m.health.faults = append(m.health.faults, now)
	m.health.why = why
}

func (m *Master) healthTick(now time.Time) {
	h := &m.health
	i := 0
	for i < len(h.faults) && now.Sub(h.faults[i]) > faultWindow {
		i++
	}
	h.faults = h.faults[i:]
	why := ""
	switch {
	case h.unanswered >= 3 && now.Sub(h.heardAt) > silentAfter:
		why = fmt.Sprintf("not answering for %.0f s", now.Sub(h.heardAt).Seconds())
	case !h.stuck.IsZero() && now.Sub(h.stuck) > stuckAfter && !m.ours():
		why = fmt.Sprintf("stuck in state %s for %.0f s with nothing keyed", m.state, now.Sub(h.stuck).Seconds())
	case len(h.faults) >= faultsToSick:
		why = fmt.Sprintf("%d faults in %v, the last: %s", len(h.faults), faultWindow, h.why)
	}
	if why == "" {
		if h.sick {
			if h.wellSince.IsZero() {
				h.wellSince = now
			}
			if now.Sub(h.wellSince) >= wellAfter {
				m.logf("master: radio well again")
				h.sick, h.nudged, h.reinited, h.faults = false, 0, false, nil
				if m.Ladder != nil {
					m.Ladder.Level = 0
				}
			}
		}
		return
	}
	h.wellSince = time.Time{}
	if !h.sick {
		h.sick = true
		m.logf("master: RADIO SICK: %s; WIRES-X is not told, transmissions wait", why)
		m.dropTransmissions()
	}
	if now.Sub(h.actAt) < nudgeEvery {
		return
	}
	if h.nudged < nudges {
		h.nudged++
		h.actAt = now
		m.logf("master: unkeying and asking the radio's state (%d of %d)", h.nudged, nudges)
		m.Write(pttOff)
		m.Write(pttOff)
		m.send("D1C0000", now)
		return
	}
	if !h.reinited {
		// WIRES-X's startup over again: M10, R6423, the version request
		// (retried while the radio resets) and the mode write. It is what
		// restarting WIRES-X does, which cleared wedges USB and repeater
		// power cycles didn't.
		h.reinited = true
		h.actAt = now
		m.logf("master: re-running WIRES-X's startup on the radio")
		m.startup()
		return
	}
	if now.Sub(h.actAt) < reinitWait {
		return
	}
	l := m.Ladder
	if m.Recover == nil || l == nil {
		return
	}
	gap := recoverGap
	if l.Level > 2 {
		gap = recoverRest
	}
	if !l.At.IsZero() && now.Sub(l.At) < gap {
		return
	}
	level := l.Level + 1
	if level > 2 {
		level = 2
	}
	l.Level, l.At = level, now
	h.actAt = now
	names := map[int]string{1: "resetting its USB device", 2: "cycling its USB port's power"}
	m.logf("master: RECOVERING the radio (%s): %s", why, names[level])
	if err := m.Recover(level); err != nil {
		m.logf("master: recovery level %d: %v", level, err)
	}
	if level == 2 {
		l.Level = 3 // rests before the next try
	}
}

// dropTransmissions forgets what was waiting for a radio that is now sick,
// unkeying it if it was keyed.
func (m *Master) dropTransmissions() {
	if m.tx != nil && m.tx.keyed && !m.tx.unkeyed {
		m.Write(pttOff)
		m.Write(pttOff)
	}
	if n := len(m.next); m.tx != nil || n > 0 {
		m.logf("master: dropping %d transmission(s)", n+map[bool]int{true: 1}[m.tx != nil])
	}
	m.tx, m.next = nil, nil
}
