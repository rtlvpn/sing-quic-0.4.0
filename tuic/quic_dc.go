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
	// QUIC-DC constants optimized for gaming and livestreaming on ADSL2+
	initMaxDatagramSizeDC = 1252
	minCwndDC             = 2 * initMaxDatagramSizeDC
	maxCwndDC             = 1000 * initMaxDatagramSizeDC
	alphaDC               = 0.1                   // Lower alpha for more stable response
	betaDC                = 0.7                   // Less aggressive window reduction
	gammaDC               = 4.0                   // Even less sensitive delay threshold for ADSL2+
	delayTargetDC         = 40 * time.Millisecond // Better for bufferbloat-prone ADSL2+ networks
	debugPrintIntervalDC  = 2

	// Hysteresis parameters to prevent oscillation
	enterCongestionThresholdMultiplier = 1.0
	exitCongestionThresholdMultiplier  = 0.7

	// Pacing parameters
	maxBurstPackets               = 4 // Reduced for more consistent pacing
	maxBurstPacingDelayMultiplier = 2
	minPacingDelay                = 100 * time.Microsecond

	// RTT validation
	minValidRTT = 1 * time.Millisecond
	maxValidRTT = 1000 * time.Millisecond

	// Bandwidth estimation
	minSamplesForBandwidthEstimate = 4
	maxBandwidthIncreasePerRTT     = 1.25 // 25% max increase per RTT

	// Jitter tolerance - ignore delay variations within this range
	jitterToleranceMs = 8.0 // Increased for ADSL2+ jitter

	// ADSL2+ specific parameters
	asymmetricRatio     = 16.0 // Typical ADSL2+ down/up ratio
	ackCongestionFactor = 0.8  // Reduce target based on upload limits

	// Line quality thresholds
	poorLineQualityThreshold = 0.3 // Below this is considered poor quality
	goodLineQualityThreshold = 0.7 // Above this is considered good quality

	// RTT reset parameters
	rttResetIntervalGood   = 60 * time.Second // Reset interval for good lines
	rttResetIntervalPoor   = 30 * time.Second // More frequent for poor lines
	rttResetPercentageGood = 1.05             // 5% increase for good lines
	rttResetPercentagePoor = 1.10             // 10% increase for poor lines
)

// CongestionState represents the current congestion control state
type CongestionState int

const (
	StateNormal CongestionState = iota
	StateEarlyCongestion
	StateRecovery
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
	owqd        float64       // One-Way Queueing Delay (seconds)
	rttMin      time.Duration // Minimum RTT observed (base propagation delay)
	rttMax      time.Duration // Maximum RTT observed
	rttLastMin  time.Duration // Recent minimum RTT (for jitter detection)
	rttSampleTs time.Time     // Timestamp of last RTT minimum update

	// Bandwidth estimation
	bkEstimate       float64       // Bandwidth estimate (bytes/sec)
	rttEstimate      time.Duration // RTT estimate
	bandwidthSamples []float64     // Recent bandwidth samples

	// State tracking
	state               CongestionState // Current congestion state
	inSlowStart         bool
	lossEvents          int
	ackCount            int
	lastAckTime         time.Time
	lastStateChangeTime time.Time // When we last changed congestion state

	// ADSL2+ specific tracking
	lineQualityIndicator float64   // 0.0 = poor, 1.0 = excellent
	rttVarianceHistory   []float64 // Recent RTT variance samples
	lastRttVariance      float64   // Last calculated RTT variance

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
		cwnd:                 minCwndDC,
		ssthresh:             math.MaxFloat64,
		maxCwnd:              maxCwndDC,
		minCwnd:              minCwndDC,
		mss:                  initMaxDatagramSizeDC,
		owqd:                 0,
		inSlowStart:          true,
		alpha:                alphaDC,
		beta:                 betaDC,
		gamma:                gammaDC,
		delayTarget:          delayTargetDC,
		debug:                debug,
		logger:               logger,
		maxDatagramSize:      initMaxDatagramSizeDC,
		state:                StateNormal,
		bandwidthSamples:     make([]float64, 0, minSamplesForBandwidthEstimate),
		lastStateChangeTime:  time.Now(),
		lineQualityIndicator: 0.5, // Start with neutral line quality
		rttVarianceHistory:   make([]float64, 0, 10),
	}

	qdc.pacer = newPacer(func() congestion.ByteCount {
		return congestion.ByteCount(qdc.getEffectiveBandwidth())
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
		c.onPacketLossEvent(len(lostPackets), eventTime)
	}

	// Process each acknowledged packet
	for _, ackInfo := range ackedPackets {
		c.processAckedPacket(ackInfo, eventTime)
	}

	// Update RTT measurements
	if c.rttStats != nil {
		rtt := c.rttStats.SmoothedRTT()
		if c.isValidRTT(rtt) {
			c.updateRTT(rtt, eventTime)
		}
	}

	// Update line quality indicator
	c.updateLineQuality()

	// Update bandwidth estimate with recent samples
	c.updateBandwidthEstimate(eventTime)

	c.lastAckTime = eventTime
	c.ackCount += len(ackedPackets)

	// Check for early congestion signal based on OWQD
	if c.shouldEnterCongestionState() && c.state == StateNormal {
		c.enterCongestionState(eventTime)
	} else if c.shouldExitCongestionState() && c.state != StateNormal {
		c.exitCongestionState(eventTime)
	} else {
		c.onAckReceived(len(ackedPackets))
	}

	// Debug logging
	if c.debug {
		now := eventTime.Unix()
		if now-c.lastPrintTime >= debugPrintIntervalDC {
			c.lastPrintTime = now
			c.debugPrint("CWND=%.0f, OWQD=%.2fms, BW=%.2f Mbps, RTT=%dms, LineQuality=%.2f, State=%d",
				c.cwnd,
				c.owqd*1000,
				c.getEffectiveBandwidth()*8/1000000, // Convert to Mbps
				c.rttEstimate.Milliseconds(),
				c.lineQualityIndicator,
				c.state)
		}
	}
}

// isValidRTT checks if an RTT measurement is valid
func (c *QUICDCController) isValidRTT(rtt time.Duration) bool {
	return rtt >= minValidRTT && rtt <= maxValidRTT
}

// processAckedPacket processes a single acknowledged packet
func (c *QUICDCController) processAckedPacket(ackInfo congestion.AckedPacketInfo, eventTime time.Time) {
	// Only update bandwidth estimate if we have valid RTT measurements
	if c.rttStats != nil && c.rttMin > 0 {
		// Calculate instantaneous bandwidth from this ACK
		if !c.lastAckTime.IsZero() {
			interAckTime := eventTime.Sub(c.lastAckTime).Seconds()
			if interAckTime > 0.001 { // Avoid very small time intervals
				instantBandwidth := float64(ackInfo.BytesAcked) / interAckTime

				// Add to bandwidth samples
				if len(c.bandwidthSamples) >= minSamplesForBandwidthEstimate {
					c.bandwidthSamples = c.bandwidthSamples[1:]
				}
				c.bandwidthSamples = append(c.bandwidthSamples, instantBandwidth)
			}
		}

		// Calculate current RTT and queueing delay
		currentRTT := c.rttStats.SmoothedRTT()
		if c.isValidRTT(currentRTT) {
			// Queueing delay is the difference between current RTT and base RTT
			queueingDelay := currentRTT - c.rttMin

			// Convert to seconds for consistency
			queueingDelaySeconds := float64(queueingDelay.Nanoseconds()) / 1e9

			// Only update OWQD if queueing delay change is significant (beyond jitter tolerance)
			// This helps ignore normal network jitter
			currentOwqd := queueingDelaySeconds / 2 // Assuming symmetric queueing

			// Adjust jitter tolerance based on line quality
			effectiveJitterTolerance := jitterToleranceMs
			if c.lineQualityIndicator < poorLineQualityThreshold {
				// Increase jitter tolerance for poor lines
				effectiveJitterTolerance *= 1.5
			}

			// Check if the change in OWQD is significant enough to consider
			if math.Abs(currentOwqd-c.owqd)*1000 > effectiveJitterTolerance || c.owqd == 0 {
				// Apply smoothing to the queueing delay estimate
				if c.owqd == 0 {
					c.owqd = currentOwqd
				} else {
					// Use adaptive smoothing based on line quality
					adaptiveAlpha := c.alpha
					if c.lineQualityIndicator < poorLineQualityThreshold {
						// Use less aggressive smoothing for poor quality lines
						adaptiveAlpha *= 0.7
					}
					c.owqd = adaptiveAlpha*currentOwqd + (1-adaptiveAlpha)*c.owqd
				}
			}
		}
	}
}

// updateBandwidthEstimate calculates a smoothed bandwidth estimate from recent samples
func (c *QUICDCController) updateBandwidthEstimate(eventTime time.Time) {
	if len(c.bandwidthSamples) < minSamplesForBandwidthEstimate {
		return
	}

	// Calculate average from samples
	var sum float64
	for _, sample := range c.bandwidthSamples {
		sum += sample
	}
	avgBandwidth := sum / float64(len(c.bandwidthSamples))

	// Apply limit on bandwidth increase to avoid overestimation
	if c.bkEstimate > 0 {
		// Adjust increase limit based on line quality
		increaseLimit := maxBandwidthIncreasePerRTT
		if c.lineQualityIndicator < poorLineQualityThreshold {
			// More conservative for poor lines
			increaseLimit = 1.15 // 15% max increase
		} else if c.lineQualityIndicator > goodLineQualityThreshold {
			// More aggressive for good lines
			increaseLimit = 1.35 // 35% max increase
		}

		maxBandwidth := c.bkEstimate * increaseLimit
		avgBandwidth = math.Min(avgBandwidth, maxBandwidth)
	}

	// Update the bandwidth estimate
	if c.bkEstimate == 0 {
		c.bkEstimate = avgBandwidth
	} else {
		// Use adaptive smoothing based on line quality
		adaptiveAlpha := c.alpha
		if c.lineQualityIndicator < poorLineQualityThreshold {
			// More conservative updates for poor lines
			adaptiveAlpha *= 0.7
		}
		c.bkEstimate = adaptiveAlpha*avgBandwidth + (1-adaptiveAlpha)*c.bkEstimate
	}
}

// updateLineQuality calculates line quality based on RTT variance
func (c *QUICDCController) updateLineQuality() {
	if c.rttMin > 0 && c.rttMax > 0 {
		// Calculate RTT variance as a ratio
		rttVariance := float64(c.rttMax-c.rttMin) / float64(c.rttMin)
		c.lastRttVariance = rttVariance

		// Add to history
		if len(c.rttVarianceHistory) >= 10 {
			c.rttVarianceHistory = c.rttVarianceHistory[1:]
		}
		c.rttVarianceHistory = append(c.rttVarianceHistory, rttVariance)

		// Calculate average variance
		var sum float64
		for _, v := range c.rttVarianceHistory {
			sum += v
		}
		avgVariance := sum / float64(len(c.rttVarianceHistory))

		// Lower variance = better line quality
		// Use a sigmoid-like function to map variance to quality indicator
		c.lineQualityIndicator = 1.0 / (1.0 + avgVariance*3)
	}
}

// getEffectiveBandwidth returns bandwidth accounting for ADSL2+ asymmetry
func (c *QUICDCController) getEffectiveBandwidth() float64 {
	baseBandwidth := c.GetBandwidth()

	// For asymmetric links like ADSL2+, effective download is limited by upload capacity
	// Each data packet needs an ACK which consumes upload bandwidth
	uploadLimitedBandwidth := baseBandwidth / asymmetricRatio * ackCongestionFactor

	// Use the minimum of the two as effective bandwidth
	effectiveBandwidth := math.Min(baseBandwidth, uploadLimitedBandwidth)

	// For very poor quality lines, be even more conservative
	if c.lineQualityIndicator < poorLineQualityThreshold/2 {
		effectiveBandwidth *= 0.9 // Additional 10% reduction
	}

	return effectiveBandwidth
}

// onPacketLossEvent handles packet loss events
func (c *QUICDCController) onPacketLossEvent(lostCount int, eventTime time.Time) {
	c.lossEvents++

	// Exit slow start on packet loss
	if c.inSlowStart {
		c.ssthresh = c.cwnd / 2
		c.inSlowStart = false
	}

	// Change state to recovery
	c.state = StateRecovery
	c.lastStateChangeTime = eventTime

	// Use bandwidth estimate for loss recovery if available
	if c.bkEstimate > 0 && c.rttEstimate > 0 && c.rttMin > 0 {
		// Calculate target window based on bandwidth-delay product
		// Use minimum RTT to avoid including queueing delay

		// Adjust BDP calculation based on line quality
		marginFactor := 1.5 // Default 50% margin
		if c.lineQualityIndicator < poorLineQualityThreshold {
			marginFactor = 1.3 // Only 30% margin for poor lines
		} else if c.lineQualityIndicator > goodLineQualityThreshold {
			marginFactor = 1.7 // 70% margin for good lines
		}

		// Use effective bandwidth that accounts for ADSL2+ asymmetry
		targetCwnd := c.getEffectiveBandwidth() * c.rttMin.Seconds() * marginFactor
		c.cwnd = math.Max(targetCwnd, float64(c.minCwnd))
	} else {
		// Traditional congestion window reduction
		c.cwnd = c.cwnd * c.beta
	}

	c.cwnd = math.Max(c.cwnd, float64(c.minCwnd))
}

// shouldEnterCongestionState checks if we should enter early congestion state
func (c *QUICDCController) shouldEnterCongestionState() bool {
	// Need valid RTT measurements
	if c.rttMin == 0 {
		return false
	}

	// Calculate delay threshold with the enter multiplier
	// Adjust gamma based on line quality
	effectiveGamma := c.gamma
	if c.lineQualityIndicator < poorLineQualityThreshold {
		// More conservative (higher threshold) for poor lines
		effectiveGamma *= 1.2
	}

	delayThreshold := effectiveGamma * enterCongestionThresholdMultiplier * c.delayTarget.Seconds()

	// Check if queueing delay exceeds threshold
	if c.owqd > delayThreshold {
		// Don't re-enter congestion state too quickly
		// Adjust time between state changes based on line quality
		minTimeFactor := 2.0
		if c.lineQualityIndicator < poorLineQualityThreshold {
			minTimeFactor = 3.0 // Longer wait for poor lines
		}

		minTimeBetweenStateChanges := time.Duration(float64(c.rttEstimate) * minTimeFactor)
		if time.Since(c.lastStateChangeTime) < minTimeBetweenStateChanges {
			return false
		}
		return true
	}

	return false
}

// shouldExitCongestionState checks if we should exit congestion state
func (c *QUICDCController) shouldExitCongestionState() bool {
	// Need valid RTT measurements
	if c.rttMin == 0 {
		return false
	}

	// Calculate delay threshold with the exit multiplier (lower than enter)
	// Adjust gamma based on line quality
	effectiveGamma := c.gamma
	if c.lineQualityIndicator < poorLineQualityThreshold {
		// More conservative for poor lines
		effectiveGamma *= 1.2
	}

	delayThreshold := effectiveGamma * exitCongestionThresholdMultiplier * c.delayTarget.Seconds()

	// Check if queueing delay is below threshold
	if c.owqd < delayThreshold {
		// Don't exit congestion state too quickly
		// Adjust time in congestion state based on line quality
		minTimeFactor := 3.0
		if c.lineQualityIndicator < poorLineQualityThreshold {
			minTimeFactor = 4.0 // Longer wait for poor lines
		}

		minTimeInCongestionState := time.Duration(float64(c.rttEstimate) * minTimeFactor)
		if time.Since(c.lastStateChangeTime) < minTimeInCongestionState {
			return false
		}
		return true
	}

	return false
}

// enterCongestionState handles early congestion detection
func (c *QUICDCController) enterCongestionState(eventTime time.Time) {
	// Exit slow start if we're in it
	if c.inSlowStart {
		c.ssthresh = c.cwnd / 2
		c.inSlowStart = false
	}

	// Change state
	c.state = StateEarlyCongestion
	c.lastStateChangeTime = eventTime

	// Reduce congestion window based on bandwidth-delay product approach
	if c.bkEstimate > 0 && c.rttMin > 0 {
		// Calculate target window based on bandwidth-delay product
		// Use minimum RTT to avoid including queueing delay

		// Adjust margin based on line quality
		marginFactor := 1.2 // Default 20% margin
		if c.lineQualityIndicator < poorLineQualityThreshold {
			marginFactor = 1.1 // Only 10% margin for poor lines
		} else if c.lineQualityIndicator > goodLineQualityThreshold {
			marginFactor = 1.3 // 30% margin for good lines
		}

		// Use effective bandwidth that accounts for ADSL2+ asymmetry
		targetCwnd := c.getEffectiveBandwidth() * c.rttMin.Seconds() * marginFactor
		c.cwnd = math.Max(targetCwnd, float64(c.minCwnd))
	} else {
		// Reduce by beta factor if BDP can't be calculated
		c.cwnd = c.cwnd * c.beta
	}

	c.cwnd = math.Max(c.cwnd, float64(c.minCwnd))

	if c.debug {
		c.debugPrint("Entering congestion state: OWQD=%.2fms, cwnd=%.0f, quality=%.2f",
			c.owqd*1000, c.cwnd, c.lineQualityIndicator)
	}
}

// exitCongestionState handles exiting congestion state
func (c *QUICDCController) exitCongestionState(eventTime time.Time) {
	c.state = StateNormal
	c.lastStateChangeTime = eventTime

	// Don't immediately increase window - let normal ACK processing handle it
	if c.debug {
		c.debugPrint("Exiting congestion state: OWQD=%.2fms, cwnd=%.0f, quality=%.2f",
			c.owqd*1000, c.cwnd, c.lineQualityIndicator)
	}
}

// onAckReceived handles normal ACK processing
func (c *QUICDCController) onAckReceived(ackCount int) {
	// Only increase window if we're not in congestion state
	if c.state == StateNormal {
		for i := 0; i < ackCount; i++ {
			if c.inSlowStart {
				// Slow start: increase cwnd by MSS for each ACK
				c.cwnd += float64(c.mss)
				if c.cwnd >= c.ssthresh {
					c.inSlowStart = false
				}
			} else {
				// Congestion avoidance: increase cwnd by MSS^2/cwnd for each ACK
				// Adjust increase rate based on line quality
				increaseFactor := 1.0
				if c.lineQualityIndicator < poorLineQualityThreshold {
					increaseFactor = 0.8 // Slower increase for poor lines
				} else if c.lineQualityIndicator > goodLineQualityThreshold {
					increaseFactor = 1.2 // Faster increase for good lines
				}

				c.cwnd += (float64(c.mss) * float64(c.mss) * increaseFactor) / c.cwnd
			}
		}
	} else if c.state == StateRecovery && time.Since(c.lastStateChangeTime) > c.rttEstimate*4 {
		// If we've been in recovery for a while, start a slow recovery
		// Adjust recovery rate based on line quality
		recoveryRate := 0.5
		if c.lineQualityIndicator < poorLineQualityThreshold {
			recoveryRate = 0.3 // Slower recovery for poor lines
		} else if c.lineQualityIndicator > goodLineQualityThreshold {
			recoveryRate = 0.7 // Faster recovery for good lines
		}

		c.cwnd += float64(c.mss) * recoveryRate / float64(ackCount)

		// Exit recovery after a while
		if time.Since(c.lastStateChangeTime) > c.rttEstimate*8 {
			c.state = StateNormal
		}
	}

	// Cap the congestion window
	// For ADSL2+, we need to consider asymmetric bandwidth
	if c.bkEstimate > 0 && c.rttEstimate > 0 {
		// Calculate max window based on effective bandwidth
		effectiveBDP := c.getEffectiveBandwidth() * c.rttEstimate.Seconds()

		// Add a margin based on line quality
		marginFactor := 2.0 // Default 2x BDP
		if c.lineQualityIndicator < poorLineQualityThreshold {
			marginFactor = 1.5 // Only 1.5x for poor lines
		} else if c.lineQualityIndicator > goodLineQualityThreshold {
			marginFactor = 2.5 // 2.5x for good lines
		}

		effectiveMaxCwnd := effectiveBDP * marginFactor
		c.cwnd = math.Min(c.cwnd, effectiveMaxCwnd)
	}

	// Apply absolute maximum
	c.cwnd = math.Min(c.cwnd, float64(c.maxCwnd))
}

// updateRTT updates RTT estimates
func (c *QUICDCController) updateRTT(rtt time.Duration, eventTime time.Time) {
	// Update minimum RTT (base propagation delay)
	if c.rttMin == 0 || rtt < c.rttMin {
		c.rttMin = rtt
		c.rttSampleTs = eventTime
	}

	// Reset minimum RTT periodically to adapt to changing network conditions
	// But only if we're not currently in a congested state
	if c.state == StateNormal {
		// Adjust reset interval based on line quality
		resetInterval := rttResetIntervalGood
		resetPercentage := rttResetPercentageGood

		if c.lineQualityIndicator < poorLineQualityThreshold {
			// More frequent resets for poor quality lines
			resetInterval = rttResetIntervalPoor
			resetPercentage = rttResetPercentagePoor
		}

		if eventTime.Sub(c.rttSampleTs) > resetInterval {
			// Keep track of the last minimum before resetting
			c.rttLastMin = c.rttMin

			// Don't completely reset, but increase slightly to allow rediscovery
			if c.rttMin > 0 {
				c.rttMin = time.Duration(float64(c.rttMin) * resetPercentage)
			}
			c.rttSampleTs = eventTime
		}
	}

	if c.rttMax == 0 || rtt > c.rttMax {
		c.rttMax = rtt
	}

	// Exponential moving average of RTT
	if c.rttEstimate == 0 {
		c.rttEstimate = rtt
	} else {
		// Use adaptive smoothing based on line quality
		adaptiveAlpha := c.alpha
		if c.lineQualityIndicator < poorLineQualityThreshold {
			// More stable RTT estimates for poor quality lines
			adaptiveAlpha *= 0.7
		}
		c.rttEstimate = time.Duration(float64(c.rttEstimate)*(1-adaptiveAlpha) + float64(rtt)*adaptiveAlpha)
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
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == StateRecovery
}

// MaybeExitSlowStart is called to possibly exit slow start mode
func (c *QUICDCController) MaybeExitSlowStart() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Exit slow start if we detect increased delay
	if c.inSlowStart && c.rttMin > 0 {
		// Adjust exit threshold based on line quality
		rttRatio := 2.0
		if c.lineQualityIndicator < poorLineQualityThreshold {
			rttRatio = 1.7 // More sensitive for poor lines
		} else if c.lineQualityIndicator > goodLineQualityThreshold {
			rttRatio = 2.5 // Less sensitive for good lines
		}

		if c.rttEstimate > time.Duration(float64(c.rttMin)*rttRatio) {
			c.ssthresh = c.cwnd
			c.inSlowStart = false

			if c.debug {
				c.debugPrint("Exiting slow start due to increased delay: RTT=%dms, minRTT=%dms, quality=%.2f",
					c.rttEstimate.Milliseconds(), c.rttMin.Milliseconds(), c.lineQualityIndicator)
			}
		}
	}
}

// OnRetransmissionTimeout is called when a retransmission timeout occurs
func (c *QUICDCController) OnRetransmissionTimeout(packetsRetransmitted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if packetsRetransmitted {
		// Reset congestion window on timeout
		c.cwnd = float64(c.minCwnd)
		c.inSlowStart = true
		c.state = StateRecovery
		c.lastStateChangeTime = time.Now()

		// For ADSL2+, RTO often indicates severe bufferbloat or line issues
		// Adjust line quality indicator downward
		c.lineQualityIndicator = math.Max(0.1, c.lineQualityIndicator*0.7)

		if c.debug {
			c.debugPrint("RTO: cwnd reset to %.0f, quality reduced to %.2f", c.cwnd, c.lineQualityIndicator)
		}
	}
}

// GetBandwidth returns the current bandwidth estimate
func (c *QUICDCController) GetBandwidth() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.bkEstimate > 0 {
		return c.bkEstimate
	}

	// If we don't have a bandwidth estimate, derive from cwnd/rtt
	if c.rttEstimate > 0 {
		return c.cwnd / c.rttEstimate.Seconds()
	}

	// Default to a conservative estimate
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

// GetLineQuality returns the current line quality indicator
// This can be useful for applications to adapt their behavior
func (c *QUICDCController) GetLineQuality() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lineQualityIndicator
}

// GetDelayStats returns current delay statistics
func (c *QUICDCController) GetDelayStats() (minRTT, currentRTT time.Duration, owqd float64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rttMin, c.rttEstimate, c.owqd
}
