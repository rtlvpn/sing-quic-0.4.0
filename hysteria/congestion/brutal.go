package congestion

import (
	"fmt"
	"time"

	"github.com/sagernet/quic-go/congestion"
	"github.com/sagernet/sing/common/logger"
)

const (
	initMaxDatagramSize = 1252

	pktInfoSlotCount           = 5 // slot index is based on seconds, so this is basically how many seconds we sample
	minSampleCount             = 50
	minAckRate                 = 0.6
	congestionWindowMultiplier = 2
	debugPrintInterval         = 2
)

var _ congestion.CongestionControlEx = &BrutalSender{}

type BrutalSender struct {
	rttStats        congestion.RTTStatsProvider
	bps             congestion.ByteCount
	maxDatagramSize congestion.ByteCount
	pacer           *pacer

	pktInfoSlots          [pktInfoSlotCount]pktInfo
	ackRate               float64
	debug                 bool
	logger                logger.Logger
	lastAckPrintTimestamp int64
}

type pktInfo struct {
	Timestamp int64
	AckCount  uint64
	LossCount uint64
}

func NewBrutalSender(bps uint64, debug bool, logger logger.Logger) *BrutalSender {
	bs := &BrutalSender{
		bps:             congestion.ByteCount(bps),
		maxDatagramSize: initMaxDatagramSize,
		ackRate:         1,
		debug:           debug,
		logger:          logger,
	}
	bs.pacer = newPacer(func() congestion.ByteCount {
		return congestion.ByteCount(float64(bs.bps) / bs.ackRate)
	})
	return bs
}

func (b *BrutalSender) SetRTTStatsProvider(rttStats congestion.RTTStatsProvider) {
	b.rttStats = rttStats
}

func (b *BrutalSender) TimeUntilSend(bytesInFlight congestion.ByteCount) time.Time {
	return b.pacer.TimeUntilSend()
}

func (b *BrutalSender) HasPacingBudget(now time.Time) bool {
	return b.pacer.Budget(now) >= b.maxDatagramSize
}

func (b *BrutalSender) CanSend(bytesInFlight congestion.ByteCount) bool {
	return bytesInFlight < b.GetCongestionWindow()
}

func (b *BrutalSender) GetCongestionWindow() congestion.ByteCount {
	rtt := b.rttStats.SmoothedRTT()
	if rtt <= 0 {
		return 10240
	}
	return congestion.ByteCount(float64(b.bps) * rtt.Seconds() * congestionWindowMultiplier / b.ackRate)
}

func (b *BrutalSender) OnPacketSent(sentTime time.Time, bytesInFlight congestion.ByteCount,
	packetNumber congestion.PacketNumber, bytes congestion.ByteCount, isRetransmittable bool,
) {
	b.pacer.SentPacket(sentTime, bytes)
}

func (b *BrutalSender) OnPacketAcked(number congestion.PacketNumber, ackedBytes congestion.ByteCount,
	priorInFlight congestion.ByteCount, eventTime time.Time,
) {
	// Stub
}

func (b *BrutalSender) OnCongestionEvent(number congestion.PacketNumber, lostBytes congestion.ByteCount,
	priorInFlight congestion.ByteCount,
) {
	// Stub
}

func (b *BrutalSender) OnCongestionEventEx(priorInFlight congestion.ByteCount, eventTime time.Time, ackedPackets []congestion.AckedPacketInfo, lostPackets []congestion.LostPacketInfo) {
	currentTimestamp := eventTime.Unix()
	slot := currentTimestamp % pktInfoSlotCount
	if b.pktInfoSlots[slot].Timestamp == currentTimestamp {
		b.pktInfoSlots[slot].LossCount += uint64(len(lostPackets))
		b.pktInfoSlots[slot].AckCount += uint64(len(ackedPackets))
	} else {
		// uninitialized slot or too old, reset
		b.pktInfoSlots[slot].Timestamp = currentTimestamp
		b.pktInfoSlots[slot].AckCount = uint64(len(ackedPackets))
		b.pktInfoSlots[slot].LossCount = uint64(len(lostPackets))
	}
	b.updateAckRate(currentTimestamp)
}

func (b *BrutalSender) SetMaxDatagramSize(size congestion.ByteCount) {
	b.maxDatagramSize = size
	b.pacer.SetMaxDatagramSize(size)
	if b.debug {
		b.debugPrint("SetMaxDatagramSize: %d", size)
	}
}

func (b *BrutalSender) updateAckRate(currentTimestamp int64) {
	minTimestamp := currentTimestamp - pktInfoSlotCount
	var ackCount, lossCount uint64
	for _, info := range b.pktInfoSlots {
		if info.Timestamp < minTimestamp {
			continue
		}
		ackCount += info.AckCount
		lossCount += info.LossCount
	}
	if ackCount+lossCount < minSampleCount {
		b.ackRate = 1
		if b.canPrintAckRate(currentTimestamp) {
			b.lastAckPrintTimestamp = currentTimestamp
			b.debugPrint("Not enough samples (total=%d, ack=%d, loss=%d, rtt=%d)",
				ackCount+lossCount, ackCount, lossCount, b.rttStats.SmoothedRTT().Milliseconds())
		}
		return
	}
	rate := float64(ackCount) / float64(ackCount+lossCount)
	if rate < minAckRate {
		b.ackRate = minAckRate
		if b.canPrintAckRate(currentTimestamp) {
			b.lastAckPrintTimestamp = currentTimestamp
			b.debugPrint("ACK rate too low: %.2f, clamped to %.2f (total=%d, ack=%d, loss=%d, rtt=%d)",
				rate, minAckRate, ackCount+lossCount, ackCount, lossCount, b.rttStats.SmoothedRTT().Milliseconds())
		}
		return
	}
	b.ackRate = rate
	if b.canPrintAckRate(currentTimestamp) {
		b.lastAckPrintTimestamp = currentTimestamp
		b.debugPrint("ACK rate: %.2f (total=%d, ack=%d, loss=%d, rtt=%d)",
			rate, ackCount+lossCount, ackCount, lossCount, b.rttStats.SmoothedRTT().Milliseconds())
	}
}

func (b *BrutalSender) InSlowStart() bool {
	return false
}

func (b *BrutalSender) InRecovery() bool {
	return false
}

func (b *BrutalSender) MaybeExitSlowStart() {}

func (b *BrutalSender) OnRetransmissionTimeout(packetsRetransmitted bool) {}

func (b *BrutalSender) canPrintAckRate(currentTimestamp int64) bool {
	return b.debug && currentTimestamp-b.lastAckPrintTimestamp >= debugPrintInterval
}

func (b *BrutalSender) debugPrint(format string, a ...any) {
	b.logger.Debug("[brutal] ", fmt.Sprintf(format, a...))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
