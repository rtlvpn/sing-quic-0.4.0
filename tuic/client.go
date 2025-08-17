package tuic

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/sagernet/quic-go"
	qtls "github.com/sagernet/sing-quic"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/baderror"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	aTLS "github.com/sagernet/sing/common/tls"
)

type ClientOptions struct {
	Context           context.Context
	Dialer            N.Dialer
	ServerAddress     M.Socksaddr
	TLSConfig         aTLS.Config
	UUID              [16]byte
	Password          string
	CongestionControl string
	UDPStream         bool
	ZeroRTTHandshake  bool
	Heartbeat         time.Duration
	// SendFakePackets enables sending fake Steam-like UDP packets before the QUIC handshake,
	// which can help bypass QoS filters or traffic shaping that might target QUIC traffic.
	// These fake packets mimic legitimate game traffic patterns to improve connection reliability.
	SendFakePackets    bool
	FakePacketDuration time.Duration
	FakeHeartbeatData  []byte
	FakeGameData       []byte
}

type Client struct {
	ctx                context.Context
	dialer             N.Dialer
	serverAddr         M.Socksaddr
	tlsConfig          aTLS.Config
	quicConfig         *quic.Config
	uuid               [16]byte
	password           string
	congestionControl  string
	udpStream          bool
	zeroRTTHandshake   bool
	heartbeat          time.Duration
	sendFakePackets    bool
	fakePacketDuration time.Duration
	fakeHeartbeatData  []byte
	fakeGameData       []byte

	connAccess sync.RWMutex
	conn       *clientQUICConnection
}

func NewClient(options ClientOptions) (*Client, error) {
	if options.Heartbeat == 0 {
		options.Heartbeat = 10 * time.Second
	}
	quicConfig := &quic.Config{
		DisablePathMTUDiscovery: !(runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "android" || runtime.GOOS == "darwin"),
		EnableDatagrams:         true,
		MaxIncomingUniStreams:   1 << 60,
	}
	switch options.CongestionControl {
	case "":
		options.CongestionControl = "cubic"
	case "cubic", "new_reno", "bbr", "brutal", "quic_dc":
	default:
		return nil, E.New("unknown congestion control algorithm: ", options.CongestionControl)
	}
	// Set default fake packet data if not provided
	fakeHeartbeatData := options.FakeHeartbeatData
	if len(fakeHeartbeatData) == 0 {
		fakeHeartbeatData = []byte{
			0xff, 0xff, 0xff, 0xff, // Steam packet header
			0x71, 0x30, 0x30, 0x30, // Heartbeat prefix
			0x01, 0x00, 0x00, 0x00, // Sequence number
			// Random payload
			0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23,
			0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23,
		}
	}

	fakeGameData := options.FakeGameData
	if len(fakeGameData) == 0 {
		fakeGameData = []byte{
			0xff, 0xff, 0xff, 0xff, // Steam packet header
			0x56, 0x41, 0x4c, 0x56, // "VALV"
			0x45, 0x53, 0x54, 0x45, // "ESTE"
			0x41, 0x4d, 0x20, 0x30, // "AM 0"
			// Random game data payload
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
			0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
			0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38,
			0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
		}
	}

	fakePacketDuration := options.FakePacketDuration
	if fakePacketDuration == 0 {
		fakePacketDuration = 7 * time.Second
	}

	return &Client{
		ctx:                options.Context,
		dialer:             options.Dialer,
		serverAddr:         options.ServerAddress,
		tlsConfig:          options.TLSConfig,
		quicConfig:         quicConfig,
		uuid:               options.UUID,
		password:           options.Password,
		congestionControl:  options.CongestionControl,
		udpStream:          options.UDPStream,
		zeroRTTHandshake:   options.ZeroRTTHandshake,
		heartbeat:          options.Heartbeat,
		sendFakePackets:    options.SendFakePackets,
		fakePacketDuration: fakePacketDuration,
		fakeHeartbeatData:  fakeHeartbeatData,
		fakeGameData:       fakeGameData,
	}, nil
}

func (c *Client) offer(ctx context.Context) (*clientQUICConnection, error) {
	c.connAccess.Lock()
	defer c.connAccess.Unlock()
	conn := c.conn
	if conn != nil && conn.active() {
		return conn, nil
	}
	conn, err := c.offerNew(ctx)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// sendFakeUDPPackets sends a series of UDP packets that mimic Steam game traffic
// to help bypass QoS filtering before establishing the actual QUIC connection
func (c *Client) sendFakeUDPPackets(udpConn net.Conn) {
	// Use configurable fake packet data
	fakeHeartbeat := make([]byte, len(c.fakeHeartbeatData))
	copy(fakeHeartbeat, c.fakeHeartbeatData)

	fakeGameData := make([]byte, len(c.fakeGameData))
	copy(fakeGameData, c.fakeGameData)

	startTime := time.Now()
	duration := c.fakePacketDuration
	sequence := uint32(1)

	for time.Since(startTime) < duration {
		// Update sequence number in heartbeat packet (if packet is long enough)
		if len(fakeHeartbeat) >= 12 {
			binary.LittleEndian.PutUint32(fakeHeartbeat[8:12], sequence)
		}

		// Send heartbeat packet
		udpConn.Write(fakeHeartbeat)
		time.Sleep(100 * time.Millisecond)

		// Vary game data slightly to look more realistic (if packet is long enough)
		if len(fakeGameData) > 8 {
			fakeGameData[8] = byte(sequence % 256)
		}
		if len(fakeGameData) > 12 {
			fakeGameData[12] = byte((sequence >> 8) % 256)
		}

		// Send game data packet
		udpConn.Write(fakeGameData)
		time.Sleep(150 * time.Millisecond)

		// Send another variation of game data (if packet is long enough)
		if len(fakeGameData) > 16 {
			fakeGameData[16] = byte((sequence >> 16) % 256)
		}
		udpConn.Write(fakeGameData)

		sequence++
		time.Sleep(200 * time.Millisecond)
	}
}

func (c *Client) offerNew(ctx context.Context) (*clientQUICConnection, error) {
	udpConn, err := c.dialer.DialContext(c.ctx, "udp", c.serverAddr)
	if err != nil {
		return nil, err
	}

	// Send fake Steam UDP packets before establishing QUIC connection
	if c.sendFakePackets {
		c.sendFakeUDPPackets(udpConn)
	}

	var quicConn quic.Connection
	if c.zeroRTTHandshake {
		quicConn, err = qtls.DialEarly(c.ctx, bufio.NewUnbindPacketConn(udpConn), udpConn.RemoteAddr(), c.tlsConfig, c.quicConfig)
	} else {
		quicConn, err = qtls.Dial(c.ctx, bufio.NewUnbindPacketConn(udpConn), udpConn.RemoteAddr(), c.tlsConfig, c.quicConfig)
	}
	if err != nil {
		udpConn.Close()
		return nil, E.Cause(err, "open connection")
	}
	setCongestion(c.ctx, quicConn, c.congestionControl)
	conn := &clientQUICConnection{
		quicConn:   quicConn,
		rawConn:    udpConn,
		connDone:   make(chan struct{}),
		udpConnMap: make(map[uint16]*udpPacketConn),
	}
	go func() {
		hErr := c.clientHandshake(quicConn)
		if hErr != nil {
			conn.closeWithError(hErr)
		}
	}()
	if c.udpStream {
		go c.loopUniStreams(conn)
	}
	go c.loopMessages(conn)
	go c.loopHeartbeats(conn)
	c.conn = conn
	return conn, nil
}

func (c *Client) clientHandshake(conn quic.Connection) error {
	authStream, err := conn.OpenUniStream()
	if err != nil {
		return E.Cause(err, "open handshake stream")
	}
	defer authStream.Close()
	handshakeState := conn.ConnectionState()
	tuicAuthToken, err := handshakeState.TLS.ExportKeyingMaterial(string(c.uuid[:]), []byte(c.password), 32)
	if err != nil {
		return E.Cause(err, "export keying material")
	}
	authRequest := buf.NewSize(AuthenticateLen)
	authRequest.WriteByte(Version)
	authRequest.WriteByte(CommandAuthenticate)
	authRequest.Write(c.uuid[:])
	authRequest.Write(tuicAuthToken)
	return common.Error(authStream.Write(authRequest.Bytes()))
}

func (c *Client) loopHeartbeats(conn *clientQUICConnection) {
	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-conn.connDone:
			return
		case <-ticker.C:
			err := conn.quicConn.SendDatagram([]byte{Version, CommandHeartbeat})
			if err != nil {
				conn.closeWithError(E.Cause(err, "send heartbeat"))
			}
		}
	}
}

func (c *Client) DialConn(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	conn, err := c.offer(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := conn.quicConn.OpenStream()
	if err != nil {
		return nil, err
	}
	return &clientConn{
		Stream:      stream,
		parent:      conn,
		destination: destination,
	}, nil
}

func (c *Client) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	conn, err := c.offer(ctx)
	if err != nil {
		return nil, err
	}
	var sessionID uint16
	clientPacketConn := newUDPPacketConn(ctx, conn.quicConn, c.udpStream, false, func() {
		conn.udpAccess.Lock()
		delete(conn.udpConnMap, sessionID)
		conn.udpAccess.Unlock()
	})
	conn.udpAccess.Lock()
	sessionID = conn.udpSessionID
	conn.udpSessionID++
	conn.udpConnMap[sessionID] = clientPacketConn
	conn.udpAccess.Unlock()
	clientPacketConn.sessionID = sessionID
	return clientPacketConn, nil
}

func (c *Client) CloseWithError(err error) error {
	c.connAccess.Lock()
	defer c.connAccess.Unlock()
	conn := c.conn
	if conn != nil {
		conn.closeWithError(err)
	}
	return nil
}

type clientQUICConnection struct {
	quicConn     quic.Connection
	rawConn      io.Closer
	closeOnce    sync.Once
	connDone     chan struct{}
	connErr      error
	udpAccess    sync.RWMutex
	udpConnMap   map[uint16]*udpPacketConn
	udpSessionID uint16
}

func (c *clientQUICConnection) active() bool {
	select {
	case <-c.quicConn.Context().Done():
		return false
	default:
	}
	select {
	case <-c.connDone:
		return false
	default:
	}
	return true
}

func (c *clientQUICConnection) closeWithError(err error) {
	c.closeOnce.Do(func() {
		c.connErr = err
		close(c.connDone)
		_ = c.quicConn.CloseWithError(0, "")
		_ = c.rawConn.Close()
	})
}

type clientConn struct {
	quic.Stream
	parent         *clientQUICConnection
	destination    M.Socksaddr
	requestWritten bool
}

func (c *clientConn) NeedHandshake() bool {
	return !c.requestWritten
}

func (c *clientConn) Read(b []byte) (n int, err error) {
	n, err = c.Stream.Read(b)
	return n, baderror.WrapQUIC(err)
}

func (c *clientConn) Write(b []byte) (n int, err error) {
	if !c.requestWritten {
		request := buf.NewSize(2 + AddressSerializer.AddrPortLen(c.destination) + len(b))
		defer request.Release()
		request.WriteByte(Version)
		request.WriteByte(CommandConnect)
		err = AddressSerializer.WriteAddrPort(request, c.destination)
		if err != nil {
			return
		}
		request.Write(b)
		_, err = c.Stream.Write(request.Bytes())
		if err != nil {
			c.parent.closeWithError(E.Cause(err, "create new connection"))
			return 0, baderror.WrapQUIC(err)
		}
		c.requestWritten = true
		return len(b), nil
	}
	n, err = c.Stream.Write(b)
	return n, baderror.WrapQUIC(err)
}

func (c *clientConn) Close() error {
	c.Stream.CancelRead(0)
	return c.Stream.Close()
}

func (c *clientConn) LocalAddr() net.Addr {
	return M.Socksaddr{}
}

func (c *clientConn) RemoteAddr() net.Addr {
	return c.destination
}
