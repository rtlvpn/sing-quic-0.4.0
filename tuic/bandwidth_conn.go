package tuic

import (
	"net"
	"sync"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// bandwidthLimitedConn wraps a net.Conn with bandwidth limiting
type bandwidthLimitedConn struct {
	net.Conn
	limiter    *BandwidthLimiter
	readBuffer []byte
	mu         sync.Mutex
}

// newBandwidthLimitedConn creates a new bandwidth-limited connection
func newBandwidthLimitedConn(conn net.Conn, limiter *BandwidthLimiter) *bandwidthLimitedConn {
	return &bandwidthLimitedConn{
		Conn:    conn,
		limiter: limiter,
	}
}

// Read implements net.Conn with download bandwidth limiting
func (c *bandwidthLimitedConn) Read(b []byte) (int, error) {
	if c.limiter == nil {
		return c.Conn.Read(b)
	}

	// Read from underlying connection
	n, err := c.Conn.Read(b)
	if n > 0 {
		// Apply download bandwidth limiting
		allowed, delay := c.limiter.LimitDownload(int64(n))
		if allowed < int64(n) {
			// If we can't read all bytes immediately, we need to handle this
			// For now, we'll just read what we're allowed and return
			if delay > 0 {
				time.Sleep(delay)
			}
			return int(allowed), err
		}

		if delay > 0 {
			time.Sleep(delay)
		}
	}

	return n, err
}

// Write implements net.Conn with upload bandwidth limiting
func (c *bandwidthLimitedConn) Write(b []byte) (int, error) {
	if c.limiter == nil {
		return c.Conn.Write(b)
	}

	if len(b) == 0 {
		return 0, nil
	}

	// Apply upload bandwidth limiting
	allowed, delay := c.limiter.LimitUpload(int64(len(b)))
	if allowed < int64(len(b)) {
		// If we can't write all bytes immediately, we need to handle this
		// For now, we'll just write what we're allowed and return
		if delay > 0 {
			time.Sleep(delay)
		}
		n, err := c.Conn.Write(b[:allowed])
		return n, err
	}

	if delay > 0 {
		time.Sleep(delay)
	}

	return c.Conn.Write(b)
}

// bandwidthLimitedPacketConn wraps N.NetPacketConn with bandwidth limiting
type bandwidthLimitedPacketConn struct {
	N.NetPacketConn
	limiter *BandwidthLimiter
	mu      sync.Mutex
}

// newBandwidthLimitedPacketConn creates a new bandwidth-limited packet connection
func newBandwidthLimitedPacketConn(conn N.NetPacketConn, limiter *BandwidthLimiter) *bandwidthLimitedPacketConn {
	return &bandwidthLimitedPacketConn{
		NetPacketConn: conn,
		limiter:       limiter,
	}
}

// ReadPacket implements N.NetPacketConn with download bandwidth limiting
func (c *bandwidthLimitedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	if c.limiter == nil {
		return c.NetPacketConn.ReadPacket(buffer)
	}

	// Read packet from underlying connection
	destination, err := c.NetPacketConn.ReadPacket(buffer)
	if err == nil && buffer.Len() > 0 {
		// Apply download bandwidth limiting
		allowed, delay := c.limiter.LimitDownload(int64(buffer.Len()))
		if allowed < int64(buffer.Len()) {
			// Truncate buffer to allowed size
			buffer.Truncate(int(allowed))
		}

		if delay > 0 {
			time.Sleep(delay)
		}
	}

	return destination, err
}

// WritePacket implements N.NetPacketConn with upload bandwidth limiting
func (c *bandwidthLimitedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if c.limiter == nil {
		return c.NetPacketConn.WritePacket(buffer, destination)
	}

	if buffer.Len() == 0 {
		return c.NetPacketConn.WritePacket(buffer, destination)
	}

	// Apply upload bandwidth limiting
	allowed, delay := c.limiter.LimitUpload(int64(buffer.Len()))
	if allowed < int64(buffer.Len()) {
		// Create a new buffer with allowed size
		limitedBuffer := buf.NewSize(int(allowed))
		defer limitedBuffer.Release()
		limitedBuffer.Write(buffer.Bytes()[:allowed])

		if delay > 0 {
			time.Sleep(delay)
		}

		return c.NetPacketConn.WritePacket(limitedBuffer, destination)
	}

	if delay > 0 {
		time.Sleep(delay)
	}

	return c.NetPacketConn.WritePacket(buffer, destination)
}

// ReadFrom implements net.PacketConn with download bandwidth limiting
func (c *bandwidthLimitedPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	if c.limiter == nil {
		return c.NetPacketConn.ReadFrom(p)
	}

	// Read packet from underlying connection
	n, addr, err = c.NetPacketConn.ReadFrom(p)
	if n > 0 {
		// Apply download bandwidth limiting
		allowed, delay := c.limiter.LimitDownload(int64(n))
		if allowed < int64(n) {
			n = int(allowed)
		}

		if delay > 0 {
			time.Sleep(delay)
		}
	}

	return n, addr, err
}

// WriteTo implements net.PacketConn with upload bandwidth limiting
func (c *bandwidthLimitedPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if c.limiter == nil {
		return c.NetPacketConn.WriteTo(p, addr)
	}

	if len(p) == 0 {
		return c.NetPacketConn.WriteTo(p, addr)
	}

	// Apply upload bandwidth limiting
	allowed, delay := c.limiter.LimitUpload(int64(len(p)))
	if allowed < int64(len(p)) {
		if delay > 0 {
			time.Sleep(delay)
		}

		n, err = c.NetPacketConn.WriteTo(p[:allowed], addr)
		return n, err
	}

	if delay > 0 {
		time.Sleep(delay)
	}

	return c.NetPacketConn.WriteTo(p, addr)
}

// bandwidthLimitedCachedConn wraps bufio.CachedConn with bandwidth limiting
type bandwidthLimitedCachedConn struct {
	*bufio.CachedConn
	limiter *BandwidthLimiter
	mu      sync.Mutex
}

// newBandwidthLimitedCachedConn creates a new bandwidth-limited cached connection
func newBandwidthLimitedCachedConn(conn net.Conn, cache *buf.Buffer, limiter *BandwidthLimiter) *bandwidthLimitedCachedConn {
	return &bandwidthLimitedCachedConn{
		CachedConn: bufio.NewCachedConn(conn, cache),
		limiter:    limiter,
	}
}

// Read implements io.Reader with download bandwidth limiting
func (c *bandwidthLimitedCachedConn) Read(p []byte) (int, error) {
	if c.limiter == nil {
		return c.CachedConn.Read(p)
	}

	// Read from underlying connection
	n, err := c.CachedConn.Read(p)
	if n > 0 {
		// Apply download bandwidth limiting
		allowed, delay := c.limiter.LimitDownload(int64(n))
		if allowed < int64(n) {
			// If we can't read all bytes immediately, we need to handle this
			// For now, we'll just read what we're allowed and return
			if delay > 0 {
				time.Sleep(delay)
			}
			return int(allowed), err
		}

		if delay > 0 {
			time.Sleep(delay)
		}
	}

	return n, err
}

// Write implements io.Writer with upload bandwidth limiting
func (c *bandwidthLimitedCachedConn) Write(p []byte) (int, error) {
	if c.limiter == nil {
		return c.CachedConn.Write(p)
	}

	if len(p) == 0 {
		return 0, nil
	}

	// Apply upload bandwidth limiting
	allowed, delay := c.limiter.LimitUpload(int64(len(p)))
	if allowed < int64(len(p)) {
		// If we can't write all bytes immediately, we need to handle this
		// For now, we'll just write what we're allowed and return
		if delay > 0 {
			time.Sleep(delay)
		}
		n, err := c.CachedConn.Write(p[:allowed])
		return n, err
	}

	if delay > 0 {
		time.Sleep(delay)
	}

	return c.CachedConn.Write(p)
}
