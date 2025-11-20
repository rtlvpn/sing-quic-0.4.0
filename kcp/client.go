package kcp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/xtaci/kcp-go/v5"
)

type ClientOptions struct {
	Context       context.Context
	Dialer        N.Dialer
	ServerAddress M.Socksaddr
	UUID          [16]byte
	Password      string
	KCPOptions    KCPOptions
	Crypt         string
	DataShard     int
	ParityShard   int
	Heartbeat     time.Duration
}

type Client struct {
	ctx         context.Context
	dialer      N.Dialer
	serverAddr  M.Socksaddr
	uuid        [16]byte
	password    string
	kcpOptions  KCPOptions
	crypt       string
	dataShard   int
	parityShard int
	heartbeat   time.Duration

	connAccess sync.Mutex
	conn       *clientConnection
}

func NewClient(options ClientOptions) (*Client, error) {
	if options.Heartbeat == 0 {
		options.Heartbeat = 10 * time.Second
	}
	if options.KCPOptions.MTU == 0 {
		options.KCPOptions = DefaultKCPOptions()
	}
	if options.Crypt == "" {
		options.Crypt = "aes"
	}

	return &Client{
		ctx:         options.Context,
		dialer:      options.Dialer,
		serverAddr:  options.ServerAddress,
		uuid:        options.UUID,
		password:    options.Password,
		kcpOptions:  options.KCPOptions,
		crypt:       options.Crypt,
		dataShard:   options.DataShard,
		parityShard: options.ParityShard,
		heartbeat:   options.Heartbeat,
	}, nil
}

func (c *Client) offer(ctx context.Context) (*clientConnection, error) {
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
	c.conn = conn
	return conn, nil
}

func (c *Client) offerNew(ctx context.Context) (*clientConnection, error) {
	// Create KCP block cipher
	var block kcp.BlockCrypt
	var err error
	if c.crypt != "" {
		key := sha256.Sum256([]byte(c.crypt))
		block, err = kcp.NewAESBlockCrypt(key[:16])
		if err != nil {
			return nil, E.Cause(err, "create cipher")
		}
	}

	// Dial KCP session
	kcpConn, err := kcp.DialWithOptions(
		c.serverAddr.String(),
		block,
		c.dataShard,
		c.parityShard,
	)
	if err != nil {
		return nil, E.Cause(err, "dial KCP")
	}

	// Configure KCP parameters
	kcpConn.SetMtu(c.kcpOptions.MTU)
	kcpConn.SetWindowSize(c.kcpOptions.SndWnd, c.kcpOptions.RcvWnd)
	kcpConn.SetNoDelay(
		c.kcpOptions.NoDelay,
		c.kcpOptions.Interval,
		c.kcpOptions.Resend,
		c.kcpOptions.NoCongestion,
	)
	kcpConn.SetStreamMode(true)
	kcpConn.SetWriteDelay(false)
	kcpConn.SetACKNoDelay(true)

	// Perform authentication
	err = c.authenticate(kcpConn)
	if err != nil {
		kcpConn.Close()
		return nil, E.Cause(err, "authenticate")
	}

	// Create connection wrapper
	conn := &clientConnection{
		client:        c,
		kcpConn:       kcpConn,
		connDone:      make(chan struct{}),
		connAccess:    sync.Mutex{},
		tcpConnMap:    make(map[uint32]*clientTCPConn),
		udpConnMap:    make(map[uint16]*clientUDPConn),
		tcpConnAccess: sync.Mutex{},
		udpConnAccess: sync.Mutex{},
		nextTCPID:     1,
		nextUDPID:     1,
	}

	// Start background handlers
	go c.handleMessages(conn)
	go c.handleHeartbeat(conn)

	return conn, nil
}

func (c *Client) authenticate(kcpConn *kcp.UDPSession) error {
	// Generate authentication token from UUID + Password
	h := sha256.New()
	h.Write(c.uuid[:])
	h.Write([]byte(c.password))
	token := h.Sum(nil)

	// Send authentication request
	authBuffer := buf.NewSize(AuthHeaderSize)
	defer authBuffer.Release()

	authBuffer.WriteByte(Version)
	authBuffer.WriteByte(CommandAuthenticate)
	authBuffer.Write(c.uuid[:])
	authBuffer.Write(token)

	_, err := kcpConn.Write(authBuffer.Bytes())
	if err != nil {
		return err
	}

	// Read authentication response (1 byte status)
	response := make([]byte, 1)
	_, err = io.ReadFull(kcpConn, response)
	if err != nil {
		return err
	}

	if response[0] != 0 {
		return E.New("authentication failed")
	}

	return nil
}

func (c *Client) handleMessages(conn *clientConnection) {
	defer conn.closeWithError(io.ErrUnexpectedEOF)

	for {
		select {
		case <-conn.connDone:
			return
		default:
		}

		// Read message header (version + command)
		header := make([]byte, 2)
		_, err := io.ReadFull(conn.kcpConn, header)
		if err != nil {
			if E.IsClosedOrCanceled(err) {
				return
			}
			conn.closeWithError(E.Cause(err, "read header"))
			return
		}

		version := header[0]
		if version != Version {
			conn.closeWithError(E.New("unsupported version: ", version))
			return
		}

		command := header[1]
		switch command {
		case CommandHeartbeat:
			// Server heartbeat response, do nothing
		case CommandConnect:
			err = c.handleTCPData(conn)
		case CommandPacket:
			err = c.handleUDPPacket(conn)
		case CommandDissociate:
			err = c.handleDissociate(conn)
		default:
			err = E.New("unknown command: ", command)
		}

		if err != nil {
			if !E.IsClosedOrCanceled(err) {
				conn.closeWithError(err)
			}
			return
		}
	}
}

func (c *Client) handleTCPData(conn *clientConnection) error {
	// Read connection ID
	connIDBytes := make([]byte, 4)
	_, err := io.ReadFull(conn.kcpConn, connIDBytes)
	if err != nil {
		return err
	}
	connID := binary.BigEndian.Uint32(connIDBytes)

	// Read data length
	lengthBytes := make([]byte, 2)
	_, err = io.ReadFull(conn.kcpConn, lengthBytes)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(lengthBytes)

	// Read data
	dataBuffer := buf.NewSize(int(length))
	_, err = dataBuffer.ReadFullFrom(conn.kcpConn, int(length))
	if err != nil {
		dataBuffer.Release()
		return err
	}

	// Find TCP connection and deliver data
	conn.tcpConnAccess.Lock()
	tcpConn, ok := conn.tcpConnMap[connID]
	conn.tcpConnAccess.Unlock()

	if ok {
		select {
		case tcpConn.data <- dataBuffer:
		default:
			dataBuffer.Release()
		}
	} else {
		dataBuffer.Release()
	}

	return nil
}

func (c *Client) handleUDPPacket(conn *clientConnection) error {
	// Read association ID
	assocIDBytes := make([]byte, 2)
	_, err := io.ReadFull(conn.kcpConn, assocIDBytes)
	if err != nil {
		return err
	}
	assocID := binary.BigEndian.Uint16(assocIDBytes)

	// Read packet length
	lengthBytes := make([]byte, 2)
	_, err = io.ReadFull(conn.kcpConn, lengthBytes)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(lengthBytes)

	// Read packet data
	packetBuffer := buf.NewSize(int(length))
	_, err = packetBuffer.ReadFullFrom(conn.kcpConn, int(length))
	if err != nil {
		packetBuffer.Release()
		return err
	}

	// Find UDP connection and deliver packet
	conn.udpConnAccess.Lock()
	udpConn, ok := conn.udpConnMap[assocID]
	conn.udpConnAccess.Unlock()

	if ok {
		select {
		case udpConn.data <- packetBuffer:
		default:
			packetBuffer.Release()
		}
	} else {
		packetBuffer.Release()
	}

	return nil
}

func (c *Client) handleDissociate(conn *clientConnection) error {
	// Read ID (try 4 bytes first for TCP, fallback to 2 bytes for UDP)
	idBytes := make([]byte, 4)
	_, err := io.ReadFull(conn.kcpConn, idBytes)
	if err != nil {
		return err
	}

	// Try TCP first
	connID := binary.BigEndian.Uint32(idBytes)
	conn.tcpConnAccess.Lock()
	tcpConn, ok := conn.tcpConnMap[connID]
	if ok {
		delete(conn.tcpConnMap, connID)
	}
	conn.tcpConnAccess.Unlock()

	if ok {
		tcpConn.closeWithError(io.EOF)
		return nil
	}

	// Try UDP with first 2 bytes
	assocID := binary.BigEndian.Uint16(idBytes[:2])
	conn.udpConnAccess.Lock()
	udpConn, ok := conn.udpConnMap[assocID]
	if ok {
		delete(conn.udpConnMap, assocID)
	}
	conn.udpConnAccess.Unlock()

	if ok {
		udpConn.closeWithError(io.EOF)
	}

	return nil
}

func (c *Client) handleHeartbeat(conn *clientConnection) {
	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-conn.connDone:
			return
		case <-ticker.C:
			// Send heartbeat
			heartbeat := []byte{Version, CommandHeartbeat}
			conn.connAccess.Lock()
			_, err := conn.kcpConn.Write(heartbeat)
			conn.connAccess.Unlock()

			if err != nil {
				conn.closeWithError(E.Cause(err, "send heartbeat"))
				return
			}
		}
	}
}

func (c *Client) DialConn(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	conn, err := c.offer(ctx)
	if err != nil {
		return nil, err
	}

	// Allocate connection ID
	conn.tcpConnAccess.Lock()
	connID := conn.nextTCPID
	conn.nextTCPID++
	conn.tcpConnAccess.Unlock()

	// Send connect command with destination
	destAddr := destination.String()
	connectBuffer := buf.NewSize(ConnectHeaderSize + 2 + len(destAddr))
	defer connectBuffer.Release()

	connectBuffer.WriteByte(Version)
	connectBuffer.WriteByte(CommandConnect)
	binary.Write(connectBuffer, binary.BigEndian, connID)
	binary.Write(connectBuffer, binary.BigEndian, uint16(len(destAddr)))
	connectBuffer.WriteString(destAddr)

	conn.connAccess.Lock()
	_, err = conn.kcpConn.Write(connectBuffer.Bytes())
	conn.connAccess.Unlock()

	if err != nil {
		return nil, err
	}

	// Create TCP connection wrapper
	tcpConn := &clientTCPConn{
		client:    c,
		conn:      conn,
		connID:    connID,
		data:      make(chan *buf.Buffer, 64),
		closeOnce: sync.Once{},
		closeChan: make(chan struct{}),
	}

	conn.tcpConnAccess.Lock()
	conn.tcpConnMap[connID] = tcpConn
	conn.tcpConnAccess.Unlock()

	return tcpConn, nil
}

func (c *Client) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, err := c.offer(ctx)
	if err != nil {
		return nil, err
	}

	// Allocate association ID
	conn.udpConnAccess.Lock()
	assocID := conn.nextUDPID
	conn.nextUDPID++
	conn.udpConnAccess.Unlock()

	// Create UDP connection wrapper
	udpConn := &clientUDPConn{
		client:      c,
		conn:        conn,
		assocID:     assocID,
		destination: destination,
		data:        make(chan *buf.Buffer, 64),
		closeOnce:   sync.Once{},
		closeChan:   make(chan struct{}),
	}

	conn.udpConnAccess.Lock()
	conn.udpConnMap[assocID] = udpConn
	conn.udpConnAccess.Unlock()

	return udpConn, nil
}

func (c *Client) CloseWithError(err error) error {
	c.connAccess.Lock()
	defer c.connAccess.Unlock()

	if c.conn != nil {
		c.conn.closeWithError(err)
	}
	return nil
}

// clientConnection represents a KCP session to the server
type clientConnection struct {
	client        *Client
	kcpConn       *kcp.UDPSession
	connDone      chan struct{}
	connErr       error
	closeOnce     sync.Once
	connAccess    sync.Mutex
	tcpConnMap    map[uint32]*clientTCPConn
	udpConnMap    map[uint16]*clientUDPConn
	nextTCPID     uint32
	nextUDPID     uint16
	tcpConnAccess sync.Mutex
	udpConnAccess sync.Mutex
}

func (c *clientConnection) active() bool {
	select {
	case <-c.connDone:
		return false
	default:
		return true
	}
}

func (c *clientConnection) closeWithError(err error) {
	c.closeOnce.Do(func() {
		c.connErr = err
		close(c.connDone)

		// Close all TCP connections
		c.tcpConnAccess.Lock()
		tcpConns := make([]*clientTCPConn, 0, len(c.tcpConnMap))
		for _, tcpConn := range c.tcpConnMap {
			tcpConns = append(tcpConns, tcpConn)
		}
		c.tcpConnMap = make(map[uint32]*clientTCPConn)
		c.tcpConnAccess.Unlock()

		for _, tcpConn := range tcpConns {
			tcpConn.closeWithError(err)
		}

		// Close all UDP connections
		c.udpConnAccess.Lock()
		udpConns := make([]*clientUDPConn, 0, len(c.udpConnMap))
		for _, udpConn := range c.udpConnMap {
			udpConns = append(udpConns, udpConn)
		}
		c.udpConnMap = make(map[uint16]*clientUDPConn)
		c.udpConnAccess.Unlock()

		for _, udpConn := range udpConns {
			udpConn.closeWithError(err)
		}

		_ = c.kcpConn.Close()
	})
}

// clientTCPConn implements net.Conn for TCP connections over KCP
type clientTCPConn struct {
	client    *Client
	conn      *clientConnection
	connID    uint32
	data      chan *buf.Buffer
	closeOnce sync.Once
	closeChan chan struct{}
}

func (c *clientTCPConn) Read(p []byte) (n int, err error) {
	select {
	case <-c.closeChan:
		return 0, net.ErrClosed
	case <-c.conn.connDone:
		return 0, c.conn.connErr
	case buffer := <-c.data:
		n = copy(p, buffer.Bytes())
		buffer.Release()
		return n, nil
	}
}

func (c *clientTCPConn) Write(p []byte) (n int, err error) {
	select {
	case <-c.closeChan:
		return 0, net.ErrClosed
	default:
	}

	// Build data message
	packet := buf.NewSize(ConnectHeaderSize + 2 + len(p))
	defer packet.Release()

	packet.WriteByte(Version)
	packet.WriteByte(CommandConnect)
	binary.Write(packet, binary.BigEndian, c.connID)
	binary.Write(packet, binary.BigEndian, uint16(len(p)))
	packet.Write(p)

	// Send data
	c.conn.connAccess.Lock()
	_, err = c.conn.kcpConn.Write(packet.Bytes())
	c.conn.connAccess.Unlock()

	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *clientTCPConn) Close() error {
	c.closeWithError(nil)
	return nil
}

func (c *clientTCPConn) closeWithError(err error) {
	c.closeOnce.Do(func() {
		close(c.closeChan)

		// Remove from connection map
		c.conn.tcpConnAccess.Lock()
		delete(c.conn.tcpConnMap, c.connID)
		c.conn.tcpConnAccess.Unlock()

		// Send dissociate message
		dissociate := buf.NewSize(2 + 4)
		dissociate.WriteByte(Version)
		dissociate.WriteByte(CommandDissociate)
		binary.Write(dissociate, binary.BigEndian, c.connID)

		c.conn.connAccess.Lock()
		_, _ = c.conn.kcpConn.Write(dissociate.Bytes())
		c.conn.connAccess.Unlock()

		dissociate.Release()

		// Drain data channel
		for {
			select {
			case buffer := <-c.data:
				buffer.Release()
			default:
				return
			}
		}
	})
}

func (c *clientTCPConn) LocalAddr() net.Addr {
	return c.conn.kcpConn.LocalAddr()
}

func (c *clientTCPConn) RemoteAddr() net.Addr {
	return c.conn.kcpConn.RemoteAddr()
}

func (c *clientTCPConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *clientTCPConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *clientTCPConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// clientUDPConn implements net.PacketConn for UDP over KCP
type clientUDPConn struct {
	client      *Client
	conn        *clientConnection
	assocID     uint16
	destination M.Socksaddr
	data        chan *buf.Buffer
	closeOnce   sync.Once
	closeChan   chan struct{}
}

func (c *clientUDPConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case <-c.closeChan:
		return 0, nil, net.ErrClosed
	case <-c.conn.connDone:
		return 0, nil, c.conn.connErr
	case buffer := <-c.data:
		n = copy(p, buffer.Bytes())
		addr = c.destination.UDPAddr()
		buffer.Release()
		return
	}
}

func (c *clientUDPConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	select {
	case <-c.closeChan:
		return 0, net.ErrClosed
	default:
	}

	// Build packet message
	packet := buf.NewSize(PacketHeaderSize + 2 + len(p))
	defer packet.Release()

	packet.WriteByte(Version)
	packet.WriteByte(CommandPacket)
	binary.Write(packet, binary.BigEndian, c.assocID)
	binary.Write(packet, binary.BigEndian, uint16(len(p)))
	packet.Write(p)

	// Send packet
	c.conn.connAccess.Lock()
	_, err = c.conn.kcpConn.Write(packet.Bytes())
	c.conn.connAccess.Unlock()

	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *clientUDPConn) Close() error {
	c.closeWithError(nil)
	return nil
}

func (c *clientUDPConn) closeWithError(err error) {
	c.closeOnce.Do(func() {
		close(c.closeChan)

		// Remove from connection map
		c.conn.udpConnAccess.Lock()
		delete(c.conn.udpConnMap, c.assocID)
		c.conn.udpConnAccess.Unlock()

		// Send dissociate message
		dissociate := buf.NewSize(2 + 2)
		dissociate.WriteByte(Version)
		dissociate.WriteByte(CommandDissociate)
		binary.Write(dissociate, binary.BigEndian, c.assocID)

		c.conn.connAccess.Lock()
		_, _ = c.conn.kcpConn.Write(dissociate.Bytes())
		c.conn.connAccess.Unlock()

		dissociate.Release()

		// Drain data channel
		for {
			select {
			case buffer := <-c.data:
				buffer.Release()
			default:
				return
			}
		}
	})
}

func (c *clientUDPConn) LocalAddr() net.Addr {
	return c.conn.kcpConn.LocalAddr()
}

func (c *clientUDPConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *clientUDPConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *clientUDPConn) SetWriteDeadline(t time.Time) error {
	return nil
}
