package tuic

import (
	"time"

	"github.com/sagernet/quic-go/congestion"
)

// The pacer implements a token bucket pacing algorithm.
type pacer struct {
	budgetAtLastSent congestion.ByteCount
	maxDatagramSize  congestion.ByteCount
	lastSentTime     time.Time
	getBandwidth     func() congestion.ByteCount // in bytes/s
}

func newPacer(getBandwidth func() congestion.ByteCount) *pacer {
	p := &pacer{
		budgetAtLastSent: maxBurstPackets * initMaxDatagramSizeDC,
		maxDatagramSize:  initMaxDatagramSizeDC,
		getBandwidth:     getBandwidth,
	}
	return p
}

func (p *pacer) SentPacket(sendTime time.Time, size congestion.ByteCount) {
	budget := p.Budget(sendTime)
	if size > budget {
		p.budgetAtLastSent = 0
	} else {
		p.budgetAtLastSent = budget - size
	}
	p.lastSentTime = sendTime
}

func (p *pacer) Budget(now time.Time) congestion.ByteCount {
	if p.lastSentTime.IsZero() {
		return p.maxBurstSize()
	}
	budget := p.budgetAtLastSent + (p.getBandwidth()*congestion.ByteCount(now.Sub(p.lastSentTime).Nanoseconds()))/1e9
	if budget < 0 { // protect against overflows
		budget = congestion.ByteCount(1<<62 - 1)
	}
	return minByteCount(p.maxBurstSize(), budget)
}

func (p *pacer) maxBurstSize() congestion.ByteCount {
	// For ADSL2+, we need to be careful with burst sizes
	// Calculate burst based on bandwidth and delay
	bandwidthBurst := congestion.ByteCount((maxBurstPacingDelayMultiplier * minPacingDelay).Nanoseconds()) * p.getBandwidth() / 1e9

	// But also cap by packet count to avoid excessive bursts on high-bandwidth connections
	packetBurst := maxBurstPackets * p.maxDatagramSize

	return maxByteCount(bandwidthBurst, packetBurst)
}

// TimeUntilSend returns when the next packet should be sent.
// It returns the zero value of time.Time if a packet can be sent immediately.
func (p *pacer) TimeUntilSend() time.Time {
	if p.budgetAtLastSent >= p.maxDatagramSize {
		return time.Time{}
	}

	// For real-time applications on ADSL2+, we want to be less strict with pacing
	// to avoid introducing unnecessary delays

	// If we have at least 60% of the required budget, send immediately
	// This helps with latency-sensitive applications
	if p.budgetAtLastSent >= p.maxDatagramSize*60/100 {
		return time.Time{}
	}

	diff := 1e9 * uint64(p.maxDatagramSize-p.budgetAtLastSent)
	bw := uint64(p.getBandwidth())
	if bw == 0 {
		return time.Time{} // Don't delay if bandwidth estimate is zero
	}

	// Calculate wait time
	d := diff / bw
	if diff%bw > 0 {
		d++
	}

	// For ADSL2+ gaming/streaming, use a shorter minimum pacing delay
	return p.lastSentTime.Add(time.Duration(d) * time.Nanosecond)
}

func (p *pacer) SetMaxDatagramSize(s congestion.ByteCount) {
	p.maxDatagramSize = s
}

func maxByteCount(a, b congestion.ByteCount) congestion.ByteCount {
	if a < b {
		return b
	}
	return a
}

func minByteCount(a, b congestion.ByteCount) congestion.ByteCount {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
