package link

import (
	"testing"
	"time"
)

// simLink drives a pacer against a link of rate bytes/s with base delay base;
// the sender offers up to offer bytes/s (0 = always has data). It returns the
// throughput reached and the queueing delay at the end.
func simLink(t *testing.T, p *pacer, rate, offer float64, base, d time.Duration) (got float64, queue time.Duration) {
	return simLinkBatched(t, p, rate, offer, base, d, 0)
}

// simLinkBatched is simLink with acks released in batches every batch, the way
// adb hands data over: in bursts.
func simLinkBatched(t *testing.T, p *pacer, rate, offer float64, base, d, batch time.Duration) (got float64, queue time.Duration) {
	t.Helper()
	const chunk = 16 << 10
	now := time.Unix(1_000_000, 0)
	start, end := now, now.Add(d)
	var sent, acked, avail uint64
	type inflight struct {
		end uint64
		due time.Time
	}
	var q []inflight
	linkFree := now
	var deliveredAtHalf uint64
	half := now.Add(d / 2)
	for now.Before(end) {
		if offer > 0 {
			avail = uint64(offer * now.Sub(start).Seconds())
		} else {
			avail = ^uint64(0)
		}
		for int(sent-acked)+chunk <= p.limit(now) && sent+chunk <= avail {
			sent += chunk
			p.sent(sent, now)
			from := linkFree
			if from.Before(now) {
				from = now
			}
			linkFree = from.Add(time.Duration(float64(chunk) / rate * float64(time.Second)))
			due := linkFree.Add(base)
			if batch > 0 {
				due = due.Truncate(batch).Add(batch)
			}
			q = append(q, inflight{sent, due})
		}
		now = now.Add(100 * time.Microsecond)
		for len(q) > 0 && !q[0].due.After(now) {
			acked = q[0].end
			p.acked(acked, now)
			q = q[1:]
		}
		if deliveredAtHalf == 0 && !now.Before(half) {
			deliveredAtHalf = acked
		}
	}
	got = float64(acked-deliveredAtHalf) / (d / 2).Seconds()
	if queue = linkFree.Sub(now); queue < 0 {
		queue = 0
	}
	return got, queue
}

// TestPacerFillsLinkWithShortQueue: on a link the sender can fill, the cap
// keeps it busy while the queue beyond us stays near base + 2*paceTarget.
func TestPacerFillsLinkWithShortQueue(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate float64 // bytes/s
		base time.Duration
	}{
		{"usb-300M-1ms", 300e6 / 8, time.Millisecond},
		{"wifi-130M-4ms", 130e6 / 8, 4 * time.Millisecond},
		{"slow-20M-10ms", 20e6 / 8, 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPacer(0)
			got, queue := simLink(t, &p, tc.rate, 0, tc.base, 4*time.Second)
			t.Logf("%.0f%% of the link, queue %v, %s", 100*got/tc.rate, queue, &p)
			if got < 0.95*tc.rate {
				t.Errorf("throughput %.1f%% of the link", 100*got/tc.rate)
			}
			// The equilibrium queue is (paceGain-1)*base + paceGain*paceTarget,
			// plus a couple of frames' transmission time (the measured base
			// includes one).
			frame := time.Duration(float64(16<<10) / tc.rate * float64(time.Second))
			if limit := tc.base + 2*paceTarget + 3*frame + time.Millisecond; queue > limit {
				t.Errorf("queue %v, want under %v", queue, limit)
			}
		})
	}
}

// TestPacerBatchedAcks: acks arriving in bursts (adb) must not read as a
// faster link: the rate estimate stays near the real one, and so does the queue.
func TestPacerBatchedAcks(t *testing.T) {
	p := newPacer(0)
	rate := 250e6 / 8
	got, queue := simLinkBatched(t, &p, rate, 0, time.Millisecond, 6*time.Second, 4*time.Millisecond)
	t.Logf("%.0f%% of the link, queue %v, %s", 100*got/rate, queue, &p)
	if est := p.maxRate(); est > 1.6*rate {
		t.Errorf("rate estimate %.0f Mbit/s for a %.0f Mbit/s link", est*8/1e6, rate*8/1e6)
	}
	if got < 0.95*rate {
		t.Errorf("throughput %.1f%% of the link", 100*got/rate)
	}
	if queue > 5*paceTarget {
		t.Errorf("queue %v", queue)
	}
}

// TestPacerFollowsOfferedRate: when the source is slower than the link, the
// cap tracks the source (so a burst can't pile up) and nothing is held back.
func TestPacerFollowsOfferedRate(t *testing.T) {
	p := newPacer(0)
	offer := 100e6 / 8.0
	got, queue := simLink(t, &p, 300e6/8, offer, time.Millisecond, 4*time.Second)
	t.Logf("%.0f%% of the offered rate, queue %v, %s", 100*got/offer, queue, &p)
	if got < 0.97*offer {
		t.Errorf("delivered %.1f%% of what the source offered", 100*got/offer)
	}
	if want := int(offer * (2*time.Millisecond + paceTarget).Seconds() * paceGain * 1.5); p.current() > want {
		t.Errorf("cap %d KB, want under %d KB for a %0.f Mbit/s source", p.current()>>10, want>>10, offer*8/1e6)
	}
}

// TestPacerProbeFindsBase: a link kept full from the first byte never shows
// its empty-queue round trip until a probe drains the queue.
func TestPacerProbeFindsBase(t *testing.T) {
	p := newPacer(0)
	base := 2 * time.Millisecond
	simLink(t, &p, 100e6/8, 0, base, 15*time.Second)
	t.Logf("%s", &p)
	frame := time.Duration(float64(16<<10) / (100e6 / 8) * float64(time.Second))
	if got := p.baseRTT(); got > base+2*frame {
		t.Fatalf("base delay %v, true base %v (+%v per frame)", got, base, frame)
	}
}

// TestPacerFixed: MaxInFlight pins the cap.
func TestPacerFixed(t *testing.T) {
	p := newPacer(300 << 10)
	simLink(t, &p, 300e6/8, 0, 2*time.Millisecond, time.Second)
	if p.current() != 300<<10 {
		t.Fatalf("fixed cap moved to %d", p.current())
	}
}

// TestPacerBaseWindow: the base delay is the shortest RTT of the last minute,
// so a long-gone short RTT stops counting.
func TestPacerBaseWindow(t *testing.T) {
	p := newPacer(0)
	now := time.Unix(1_000_000, 0)
	if b := p.noteBase(2*time.Millisecond, now); b != 2*time.Millisecond {
		t.Fatalf("base %v", b)
	}
	if b := p.noteBase(8*time.Millisecond, now.Add(30*time.Second)); b != 2*time.Millisecond {
		t.Fatalf("base after 30 s = %v, want the 2 ms from before", b)
	}
	if b := p.noteBase(8*time.Millisecond, now.Add(2*time.Minute)); b != 8*time.Millisecond {
		t.Fatalf("base after 2 min = %v, want 8 ms", b)
	}
}
