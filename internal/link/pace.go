package link

import (
	"fmt"
	"time"
)

// pacer sizes how many bytes may be in flight (sent, not yet acknowledged)
// so the backlog waits in the Resumable, where datagrams can overtake it,
// rather than in socket, adbd or adb-server buffers, where they can't.
//
// It works like BBR's estimate of the path: from each acknowledgement it
// takes a round trip and a delivery rate; the shortest round trip of the last
// minute is the path's base delay, the highest delivery rate of the last two
// seconds its usable rate, and the cap is twice that rate times the base
// delay plus paceTarget, so only about base + 2*paceTarget of data queues
// anywhere beyond us.
// A delay-only controller (LEDBAT-like) does not fit this path: on USB the adb
// path's ack jitter (a few ms) reads as queueing and keeps the cap at its floor.
//
// A link kept full never shows its empty-queue round trip, and an inflated
// base delay would keep the queue it measures. So, like BBR's ProbeRTT, every
// paceProbeEvery the cap drops to paceMin until the queue has drained, and
// the round trips seen then replace the base delay.
//
// All methods run under the Resumable's mutex; time comes from the caller.
type pacer struct {
	fixed int // > 0: a fixed cap, no adaptation (ResumableConfig.MaxInFlight)

	samples []paceSample // sends not yet acknowledged, in offset order

	base     [paceBaseSlots]time.Duration // shortest RTT per 10 s slot (0 = none)
	baseSlot int64
	rate     [paceRateSlots]float64 // highest delivery rate per 500 ms slot, bytes/s
	rateSlot int64
	lastRTT  time.Duration
	acks     []paceAck // acks of the last paceRateWindow (and the one just before it)

	nextProbe time.Time // zero until the first sample
	probing   bool
	probeEnd  time.Time
	probeMin  time.Duration
}

type paceSample struct {
	end uint64    // wire offset just past what was sent
	at  time.Time // when it was sent
}

type paceAck struct {
	at    time.Time
	acked uint64
}

const (
	paceInitial = 1 << 20 // until the first estimates
	// paceMin keeps two small frames in flight; acks come back per frame, so
	// that is enough to keep a slow link moving without queueing on it.
	paceMin = 32 << 10
	paceMax = 16 << 20
	// paceTarget is added to the base delay before the gain: room for ack
	// batching on a path whose base delay is a millisecond. In tests, 4 ms
	// kept a datagram about 10 ms behind a full download on USB and Wi-Fi;
	// 8 ms roughly doubled that for about 10 % more Wi-Fi throughput.
	paceTarget = 4 * time.Millisecond
	// paceGain is BBR's cwnd gain. Below 2, a path whose loaded round trip
	// sits above its base (adb batches) would read a lower rate every round
	// and shrink the cap to its floor. The queue this allows is about
	// base + 2*paceTarget at the link's rate.
	paceGain = 2.0

	paceSlot       = 10 * time.Second
	paceBaseSlots  = 6 // base delay = shortest RTT of the last minute
	paceRateSlot   = 500 * time.Millisecond
	paceRateSlots  = 4                     // rate = highest windowed delivery rate of the last 2 s
	paceRateWindow = 50 * time.Millisecond // delivery rates are measured over this much time

	paceFirstProbe = 2 * time.Second
	paceProbeEvery = 10 * time.Second
)

func newPacer(fixed int) pacer { return pacer{fixed: fixed} }

// reset starts over, for a new transport: its delay and rate are its own.
func (p *pacer) reset() { *p = pacer{fixed: p.fixed} }

// limit is the cap on bytes in flight at now; it may start a probe.
func (p *pacer) limit(now time.Time) int {
	if p.fixed > 0 {
		return p.fixed
	}
	if !p.probing && !p.nextProbe.IsZero() && !now.Before(p.nextProbe) {
		// Long enough for what is queued now to drain, then a few round trips.
		p.probing, p.probeMin = true, 0
		p.probeEnd = now.Add(2*p.baseRTT() + 10*time.Millisecond)
	}
	return p.current()
}

// current is the cap, without starting a probe.
func (p *pacer) current() int {
	if p.fixed > 0 {
		return p.fixed
	}
	if p.probing {
		return paceMin
	}
	base, rate := p.baseRTT(), p.maxRate()
	if base == 0 || rate == 0 {
		return paceInitial
	}
	c := rate * (base + paceTarget).Seconds() * paceGain
	return max(paceMin, min(paceMax, int(c)))
}

// sent records that the wire up to end was handed to the transport at at.
func (p *pacer) sent(end uint64, at time.Time) {
	p.samples = append(p.samples, paceSample{end, at})
}

// acked takes an acknowledgement of the wire up to a at now.
func (p *pacer) acked(a uint64, now time.Time) {
	i := 0
	for i < len(p.samples) && p.samples[i].end <= a {
		i++
	}
	p.noteAck(a, now)
	if i == 0 {
		return
	}
	s := p.samples[i-1] // the newest send this ack covers: its round trip
	p.samples = p.samples[i:]
	if len(p.samples) == 0 {
		p.samples = nil
	}
	rtt := now.Sub(s.at)
	if rtt <= 0 {
		return
	}
	p.lastRTT = rtt
	if p.probing {
		// The rate is held down on purpose now; only the delay counts.
		if p.probeMin == 0 || rtt < p.probeMin {
			p.probeMin = rtt
		}
		if !now.Before(p.probeEnd) {
			p.base = [paceBaseSlots]time.Duration{}
			p.baseSlot = 0
			p.noteBase(p.probeMin, now)
			p.probing = false
			p.nextProbe = now.Add(paceProbeEvery)
		}
		return
	}
	if p.nextProbe.IsZero() {
		p.nextProbe = now.Add(paceFirstProbe)
	}
	p.noteBase(rtt, now)
}

// noteAck keeps the acks of the last paceRateWindow and records the rate
// over that window. Per-ack rates (BBR's) don't survive the adb path: it
// releases and acknowledges megabytes within a millisecond or two, which read
// as gigabits; over tens of milliseconds those bursts average out.
func (p *pacer) noteAck(a uint64, now time.Time) {
	p.acks = append(p.acks, paceAck{now, a})
	cut := now.Add(-paceRateWindow)
	i := 0
	for i+1 < len(p.acks) && !p.acks[i+1].at.After(cut) {
		i++ // keep the newest ack at or before the window start
	}
	p.acks = p.acks[i:]
	from := p.acks[0]
	if span := now.Sub(from.at); span >= paceRateWindow && !p.probing {
		p.noteRate(float64(a-from.acked)/span.Seconds(), now)
	}
}

// noteBase records rtt in the current slot and returns the shortest RTT of the window.
func (p *pacer) noteBase(rtt time.Duration, now time.Time) time.Duration {
	slot := now.UnixNano() / int64(paceSlot)
	if p.baseSlot != 0 {
		for s := p.baseSlot + 1; s <= slot && s <= p.baseSlot+paceBaseSlots; s++ {
			p.base[s%paceBaseSlots] = 0
		}
	}
	p.baseSlot = slot
	if b := &p.base[slot%paceBaseSlots]; *b == 0 || rtt < *b {
		*b = rtt
	}
	return p.baseRTT()
}

func (p *pacer) noteRate(r float64, now time.Time) {
	slot := now.UnixNano() / int64(paceRateSlot)
	if p.rateSlot != 0 {
		for s := p.rateSlot + 1; s <= slot && s <= p.rateSlot+paceRateSlots; s++ {
			p.rate[s%paceRateSlots] = 0
		}
	}
	p.rateSlot = slot
	if v := &p.rate[slot%paceRateSlots]; r > *v {
		*v = r
	}
}

func (p *pacer) baseRTT() time.Duration {
	var base time.Duration
	for _, b := range p.base {
		if b > 0 && (base == 0 || b < base) {
			base = b
		}
	}
	return base
}

func (p *pacer) maxRate() float64 {
	var r float64
	for _, v := range p.rate {
		r = max(r, v)
	}
	return r
}

func (p *pacer) String() string {
	return fmt.Sprintf("inflight_cap=%dKB base_rtt=%s rtt=%s rate=%.0fMbit", p.current()>>10,
		p.baseRTT().Round(100*time.Microsecond), p.lastRTT.Round(100*time.Microsecond), p.maxRate()*8/1e6)
}
