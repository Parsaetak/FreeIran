package provider

import "time"

// probeSchedule is the adaptive readiness-probe cadence shared by the
// provider engines (v0.9.9 §14): probe immediately, then back off —
// short steps first, bounded at maxInterval. The fixed 200/250 ms
// tickers paid the full interval even for cores that open their
// listeners within the first tens of milliseconds, and allocated a
// fresh channel per wake through time.Tick's old contract; the
// schedule below keeps one reusable timer and reaches the same bounded
// steady-state cadence.
type probeSchedule struct {
	attempt int
	timer   *time.Timer
}

func newProbeSchedule() *probeSchedule {
	return &probeSchedule{timer: time.NewTimer(0)}
}

// stop releases the reusable timer.
func (s *probeSchedule) stop() {
	if s != nil && s.timer != nil {
		s.timer.Stop()
	}
}

// C returns the schedule's wait channel (fires immediately for the
// first probe, then after the adaptive delay of the previous step).
func (s *probeSchedule) C() <-chan time.Time {
	return s.timer.C
}

// next arms the following adaptive delay.
func (s *probeSchedule) next() {
	delays := []time.Duration{
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
	}

	delay := 200 * time.Millisecond // bounded steady-state cadence
	if s.attempt < len(delays) {
		delay = delays[s.attempt]
	}

	s.attempt++
	s.timer.Reset(delay)
}
