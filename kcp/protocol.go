package kcp

const (
	Version = 1
)

// Protocol commands
const (
	CommandAuthenticate = iota
	CommandConnect
	CommandPacket
	CommandDissociate
	CommandHeartbeat
)

const (
	AuthHeaderSize = 2 + 16 + 32 // version + command + uuid + token
	PacketHeaderSize = 2 + 2     // version + command + assocID
	ConnectHeaderSize = 2 + 4    // version + command + assocID
)

// KCPOptions contains KCP protocol tuning parameters
type KCPOptions struct {
	// MTU is the Maximum Transmission Unit
	// Default: 1350 (safe for most networks)
	MTU int `json:"mtu,omitempty"`

	// SndWnd is the send window size (number of packets)
	// Default: 128
	SndWnd int `json:"snd_wnd,omitempty"`

	// RcvWnd is the receive window size (number of packets)
	// Default: 512
	RcvWnd int `json:"rcv_wnd,omitempty"`

	// NoDelay controls delay settings
	// 0: normal mode (similar to TCP)
	// 1: no delay mode (faster, more CPU)
	// Default: 1
	NoDelay int `json:"nodelay,omitempty"`

	// Interval is the internal update interval in milliseconds
	// Lower values = lower latency, higher CPU usage
	// Default: 20
	Interval int `json:"interval,omitempty"`

	// Resend is the fast resend mode
	// 0: disabled
	// 1: fast resend enabled
	// 2: early resend
	// Default: 2
	Resend int `json:"resend,omitempty"`

	// NoCongestion disables congestion control
	// 0: normal congestion control
	// 1: disable congestion control
	// Default: 1
	NoCongestion int `json:"nc,omitempty"`
}

// DefaultKCPOptions returns optimized defaults for low-latency scenarios
func DefaultKCPOptions() KCPOptions {
	return KCPOptions{
		MTU:          1350,
		SndWnd:       128,
		RcvWnd:       512,
		NoDelay:      1,
		Interval:     20,
		Resend:       2,
		NoCongestion: 1,
	}
}

// ConservativeKCPOptions returns settings optimized for stability over performance
func ConservativeKCPOptions() KCPOptions {
	return KCPOptions{
		MTU:          1350,
		SndWnd:       256,
		RcvWnd:       1024,
		NoDelay:      0,
		Interval:     40,
		Resend:       1,
		NoCongestion: 0,
	}
}

// AggressiveKCPOptions returns settings optimized for maximum speed on good networks
func AggressiveKCPOptions() KCPOptions {
	return KCPOptions{
		MTU:          1400,
		SndWnd:       512,
		RcvWnd:       2048,
		NoDelay:      1,
		Interval:     10,
		Resend:       2,
		NoCongestion: 1,
	}
}
