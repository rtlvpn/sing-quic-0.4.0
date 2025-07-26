package tuic

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/sagernet/quic-go/congestion"
	"github.com/sagernet/sing/common/logger"
)

const (
	// QUIC-DC constants
	initMaxDatagramSizeDC = 1252
	minCwndDC             = 2 * initMaxDatagramSizeDC
	maxCwndDC             = 1000 * initMaxDatagramSizeDC
	alphaDC               = 0.125                // OWQD smoothing factor
	betaDC                = 0.5                  // Congestion response factor
	gammaDC               = 2.0                  // Delay threshold multiplier
	delayTargetDC         = 5 * time.Millisecond // Target 5ms queueing delay
	debugPrintIntervalDC  = 2
)

var _ congestion.CongestionControlEx = &QUICDCController{}

// QUICDCController implements the QUIC-DC congestion control algorithm
type QUICDCController struct {
	mu sync.RWMutex

	// Congestion window parameters
	cwnd     float64 // Current congestion window (bytes)
	ssthresh float64 // Slow start threshold
	maxCwnd  float64 // Maximum congestion window
	minCwnd  float64 // Minimum congestion window
	mss      float64 // Maximum segment size

	// Delay tracking
	owqd    float64       // One-Way Queueing Delay
	prevOWD float64       // Previous One-Way Delay
	rttMin  time.Duration // Minimum RTT observed
	rttMax  time.Duration // Maximum RTT observed

	// Bandwidth estimation
	bkEstimate  float64       // Bandwidth estimate (bytes/sec)
	rttEstimate time.Duration // RTT estimate

	// State tracking
	inSlowStart bool
	lossEvents  int
	ackCount    int
	lastAckTime time.Time

	// Algorithm parameters
	alpha       float64       // OWQD smoothing factor
	beta        float64       // Congestion response factor
	gamma       float64       // Delay threshold multiplier
	delayTarget time.Duration // Target queueing delay

	// RTT stats provider
	rttStats congestion.RTTStatsProvider

	// Pacing
	pacer           *pacer
	maxDatagramSize congestion.ByteCount

	// Debug
	debug         bool
	logger        logger.Logger
	lastPrintTime int64
}

// NewQUICDCController creates a new QUIC-DC congestion controller
func NewQUICDCController(debug bool, logger logger.Logger) *QUICDCController {
	qdc := &QUICDCController{
		cwnd:            minCwndDC,
		ssthresh:        math.MaxFloat64,
		maxCwnd:         maxCwndDC,
		minCwnd:         minCwndDC,
		mss:             initMaxDatagramSizeDC,
		owqd:            0,
		prevOWD:         0,
		inSlowStart:     true,
		alpha:           alphaDC,
		beta:            betaDC,
		gamma:           gammaDC,
		delayTarget:     delayTargetDC,
		debug:           debug,
		logger:          logger,
		maxDatagramSize: initMaxDatagramSizeDC,
	}

	qdc.pacer = newPacer(func() congestion.ByteCount {
		return congestion.ByteCount(qdc.GetBandwidth())
	})

	return qdc
}

// SetRTTStatsProvider sets the RTT stats provider
func (c *QUICDCController) SetRTTStatsProvider(rttStats congestion.RTTStatsProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rttStats = rttStats
}

// TimeUntilSend returns the time when the next packet can be sent
func (c *QUICDCController) TimeUntilSend(bytesInFlight congestion.ByteCount) time.Time {
	return c.pacer.TimeUntilSend()
}

// HasPacingBudget returns whether there's enough budget to send a packet
func (c *QUICDCController) HasPacingBudget(now time.Time) bool {
	return c.pacer.Budget(now) >= c.maxDatagramSize
}

// CanSend returns whether more packets can be sent based on current window
func (c *QUICDCController) CanSend(bytesInFlight congestion.ByteCount) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return bytesInFlight < congestion.ByteCount(c.cwnd)
}

// GetCongestionWindow returns the current congestion window size
func (c *QUICDCController) GetCongestionWindow() congestion.ByteCount {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return congestion.ByteCount(c.cwnd)
}

// OnPacketSent is called when a packet is sent
func (c *QUICDCController) OnPacketSent(sentTime time.Time, bytesInFlight congestion.ByteCount,
	packetNumber congestion.PacketNumber, bytes congestion.ByteCount, isRetransmittable bool,
) {
	c.pacer.SentPacket(sentTime, bytes)
}

// OnPacketAcked is called when a packet is acknowledged
func (c *QUICDCController) OnPacketAcked(number congestion.PacketNumber, ackedBytes congestion.ByteCount,
	priorInFlight congestion.ByteCount, eventTime time.Time,
) {
	// This is implemented in OnCongestionEventEx
}

// OnCongestionEvent is called when congestion is detected
func (c *QUICDCController) OnCongestionEvent(number congestion.PacketNumber, lostBytes congestion.ByteCount,
	priorInFlight congestion.ByteCount,
) {
	// This is implemented in OnCongestionEventEx
}

// OnCongestionEventEx is called for both acknowledgments and losses
func (c *QUICDCController) OnCongestionEventEx(
	priorInFlight congestion.ByteCount,
	eventTime time.Time,
	ackedPackets []congestion.AckedPacketInfo,
	lostPackets []congestion.LostPacketInfo,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(lostPackets) > 0 {
		c.onPacketLossEvent(len(lostPackets))
	}

	// Process each acknowledged packet
	for _, ackInfo := range ackedPackets {
		c.processAckedPacket(ackInfo, eventTime)
	}

	// Update RTT measurements
	if c.rttStats != nil {
		rtt := c.rttStats.SmoothedRTT()
		if rtt > 0 {
			c.updateRTT(rtt)
		}
	}

	c.lastAckTime = eventTime
	c.ackCount += len(ackedPackets)

	// Check for early congestion signal based on OWQD
	if c.isEarlyCongestionDetected() {
		c.onEarlyCongestion()
	} else {
		c.onAckReceived(len(ackedPackets))
	}

	// Debug logging
	if c.debug {
		now := eventTime.Unix()
		if now-c.lastPrintTime >= debugPrintIntervalDC {
			c.lastPrintTime = now
			c.debugPrint("CWND=%.0f, OWQD=%.3fms, BW=%.2f Mbps, RTT=%dms, SS=%v",
				c.cwnd,
				c.owqd*1000,
				c.GetBandwidth()*8/1000000, // Convert to Mbps
				c.rttEstimate.Milliseconds(),
				c.inSlowStart)
		}
	}
}

// processAckedPacket processes a single acknowledged packet
func (c *QUICDCController) processAckedPacket(ackInfo congestion.AckedPacketInfo, eventTime time.Time) {
	// Since we don't have direct access to the packet's send time in AckedPacketInfo,
	// we'll use RTT measurements to estimate one-way delay
	if c.rttStats != nil {
		rtt := c.rttStats.SmoothedRTT()

		// Estimate one-way delay as half of RTT (assumes symmetric path)
		owd := float64(rtt.Nanoseconds()) / 2e9 // Half RTT in seconds as OWD estimate

		// Calculate one-way delay variation
		owdv := owd - c.prevOWD
		c.prevOWD = owd

		// Update one-way queueing delay
		c.owqd = c.owqd*(1-c.alpha) + owdv*c.alpha
		if c.owqd < 0 {
			c.owqd = 0 // OWQD can't be negative
		}
	}

	// Update bandwidth estimate based on packet acknowledgment
	if !c.lastAckTime.IsZero() {
		interAckTime := eventTime.Sub(c.lastAckTime).Seconds()
		if interAckTime > 0 {
			instantBandwidth := float64(ackInfo.BytesAcked) / interAckTime
			if c.bkEstimate == 0 {
				c.bkEstimate = instantBandwidth
			} else {
				c.bkEstimate = c.alpha*instantBandwidth + (1-c.alpha)*c.bkEstimate
			}
		}
	}
}

// onPacketLossEvent handles packet loss events
func (c *QUICDCController) onPacketLossEvent(lostCount int) {
	c.lossEvents++

	if c.inSlowStart {
		c.ssthresh = c.cwnd / 2
		c.inSlowStart = false
	}

	// Use bandwidth estimate for loss recovery if available
	if c.bkEstimate > 0 && c.rttEstimate > 0 {
		targetCwnd := c.bkEstimate * c.rttEstimate.Seconds()
		c.cwnd = math.Max(targetCwnd, float64(c.minCwnd))
	} else {
		// Traditional halving of the congestion window
		c.cwnd = c.cwnd * c.beta
	}

	c.cwnd = math.Max(c.cwnd, float64(c.minCwnd))
}

// isEarlyCongestionDetected checks if OWQD indicates early congestion
func (c *QUICDCController) isEarlyCongestionDetected() bool {
	if c.rttMin == 0 {
		return false
	}

	// Use OWQD as early congestion signal
	delayThreshold := c.gamma * c.delayTarget.Seconds()
	return c.owqd > delayThreshold
}

// onEarlyCongestion handles early congestion detection
func (c *QUICDCController) onEarlyCongestion() {
	if c.inSlowStart {
		c.ssthresh = c.cwnd / 2
		c.inSlowStart = false
	}

	// Reduce congestion window based on bandwidth-delay product approach
	if c.bkEstimate > 0 && c.rttEstimate > 0 {
		targetCwnd := c.bkEstimate * c.rttEstimate.Seconds()
		c.cwnd = math.Max(targetCwnd, float64(c.minCwnd))
	} else {
		// Reduce by beta factor if BDP can't be calculated
		c.cwnd = c.cwnd * c.beta
	}

	c.cwnd = math.Max(c.cwnd, float64(c.minCwnd))
}

// onAckReceived handles normal ACK processing
func (c *QUICDCController) onAckReceived(ackCount int) {
	for i := 0; i < ackCount; i++ {
		if c.inSlowStart {
			// Slow start: increase cwnd by MSS for each ACK
			c.cwnd += float64(c.mss)
			if c.cwnd >= c.ssthresh {
				c.inSlowStart = false
			}
		} else {
			// Congestion avoidance: increase cwnd by MSS^2/cwnd for each ACK
			c.cwnd += (float64(c.mss) * float64(c.mss)) / c.cwnd
		}
	}

	c.cwnd = math.Min(c.cwnd, float64(c.maxCwnd))
}

// updateRTT updates RTT estimates
func (c *QUICDCController) updateRTT(rtt time.Duration) {
	if c.rttMin == 0 || rtt < c.rttMin {
		c.rttMin = rtt
	}

	if c.rttMax == 0 || rtt > c.rttMax {
		c.rttMax = rtt
	}

	// Exponential moving average of RTT
	if c.rttEstimate == 0 {
		c.rttEstimate = rtt
	} else {
		c.rttEstimate = time.Duration(float64(c.rttEstimate)*(1-c.alpha) + float64(rtt)*c.alpha)
	}
}

// SetMaxDatagramSize sets the maximum datagram size
func (c *QUICDCController) SetMaxDatagramSize(size congestion.ByteCount) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maxDatagramSize = size
	c.mss = float64(size)
	c.pacer.SetMaxDatagramSize(size)

	// Also update the min/max CWND proportionally
	c.minCwnd = 2 * float64(size)
	c.maxCwnd = 1000 * float64(size)

	if c.debug {
		c.debugPrint("SetMaxDatagramSize: %d", size)
	}
}

// InSlowStart returns whether the controller is in slow start mode
func (c *QUICDCController) InSlowStart() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.inSlowStart
}

// InRecovery returns whether the controller is in recovery mode
func (c *QUICDCController) InRecovery() bool {
	return false // QUIC-DC doesn't use a specific recovery state
}

// MaybeExitSlowStart is called to possibly exit slow start mode
func (c *QUICDCController) MaybeExitSlowStart() {
	// Not needed for QUIC-DC implementation
}

// OnRetransmissionTimeout is called when a retransmission timeout occurs
func (c *QUICDCController) OnRetransmissionTimeout(packetsRetransmitted bool) {
	// Not used in this implementation
}

// GetBandwidth returns the current bandwidth estimate
func (c *QUICDCController) GetBandwidth() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.bkEstimate > 0 && c.rttEstimate > 0 {
		return c.bkEstimate
	}

	if c.rttEstimate > 0 {
		return c.cwnd / c.rttEstimate.Seconds()
	}

	return c.cwnd / 0.1 // Assume 100ms RTT if no estimate available
}

// SetParameters allows tuning of algorithm parameters
func (c *QUICDCController) SetParameters(alpha, beta, gamma float64, delayTarget time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if alpha > 0 && alpha < 1 {
		c.alpha = alpha
	}

	if beta > 0 && beta < 1 {
		c.beta = beta
	}

	if gamma > 0 {
		c.gamma = gamma
	}

	if delayTarget > 0 {
		c.delayTarget = delayTarget
	}
}

// debugPrint prints debug information
func (c *QUICDCController) debugPrint(format string, a ...interface{}) {
	if c.debug && c.logger != nil {
		c.logger.Debug("[quic-dc] ", fmt.Sprintf(format, a...))
	}
}
