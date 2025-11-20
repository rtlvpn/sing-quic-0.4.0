package kcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/xtaci/kcp-go/v5"
)

type ServiceOptions struct {
	Context     context.Context
	Logger      logger.Logger
	KCPOptions  KCPOptions
	Crypt       string
	DataShard   int
	ParityShard int
	Heartbeat   time.Duration
	Handler     ServerHandler
}

type ServerHandler interface {
	N.TCPConnectionHandlerEx
	N.UDPConnectionHandlerEx
}

type Service[U comparable] struct {
	ctx         context.Context
	logger      logger.Logger
	kcpOptions  KCPOptions
	crypt       string
	dataShard   int
	parityShard int
	heartbeat   time.Duration
	handler     ServerHandler

	userMap     map[[16]byte]U
	passwordMap map[[16]byte]string
	userAccess  sync.RWMutex

	listener net.PacketConn
}

func NewService[U comparable](options ServiceOptions) (*Service[U], error) {
	if options.Logger == nil {
		options.Logger = logger.NOP()
	}
	if options.Heartbeat == 0 {
		options.Heartbeat = 10 * time.Second
	}
	if options.KCPOptions.MTU == 0 {
		options.KCPOptions = DefaultKCPOptions()
	}
	if options.Crypt == "" {
		options.Crypt = "aes"
	}

	return &Service[U]{
		ctx:         options.Context,
		logger:      options.Logger,
		kcpOptions:  options.KCPOptions,
		crypt:       options.Crypt,
		dataShard:   options.DataShard,
		parityShard: options.ParityShard,
		heartbeat:   options.Heartbeat,
		handler:     options.Handler,
		userMap:     make(map[[16]byte]U),
		passwordMap: make(map[[16]byte]string),
	}, nil
}

func (s *Service[U]) UpdateUsers(userList []U, uuidList [][16]byte, passwordList []string) {
	userMap := make(map[[16]byte]U)
	passwordMap := make(map[[16]byte]string)

	for i, user := range userList {
		userMap[uuidList[i]] = user
		passwordMap[uuidList[i]] = passwordList[i]
	}

	s.userAccess.Lock()
	s.userMap = userMap
	s.passwordMap = passwordMap
	s.userAccess.Unlock()
}

func (s *Service[U]) Start(conn net.PacketConn) error {
	if s.listener != nil {
		return E.New("service already started")
	}

	s.listener = conn

	// Create KCP block cipher
	var block kcp.BlockCrypt
	var err error
	if s.crypt != "" {
		key := sha256.Sum256([]byte(s.crypt))
		block, err = kcp.NewAESBlockCrypt(key[:16])
		if err != nil {
			return E.Cause(err, "create cipher")
		}
	}

	// Create KCP listener
	kcpListener, err := kcp.ServeConn(block, s.dataShard, s.parityShard, conn)
	if err != nil {
		return E.Cause(err, "create KCP listener")
	}

	// Set read/write buffers
	if err := kcpListener.SetReadBuffer(4 * 1024 * 1024); err != nil {
		s.logger.Warn("failed to set read buffer: ", err)
	}
	if err := kcpListener.SetWriteBuffer(4 * 1024 * 1024); err != nil {
		s.logger.Warn("failed to set write buffer: ", err)
	}
	if err := kcpListener.SetDSCP(46); err != nil {
		s.logger.Warn("failed to set DSCP: ", err)
	}

	go s.acceptLoop(kcpListener)
	return nil
}

func (s *Service[U]) Close() error {
	return common.Close(s.listener)
}

func (s *Service[U]) acceptLoop(listener *kcp.Listener) {
	for {
		kcpConn, err := listener.AcceptKCP()
		if err != nil {
			if E.IsClosedOrCanceled(err) {
				s.logger.Debug("listener closed")
			} else {
				s.logger.Error(E.Cause(err, "accept connection"))
			}
			return
		}

		// Configure KCP session
		kcpConn.SetMtu(s.kcpOptions.MTU)
		kcpConn.SetWindowSize(s.kcpOptions.SndWnd, s.kcpOptions.RcvWnd)
		kcpConn.SetNoDelay(
			s.kcpOptions.NoDelay,
			s.kcpOptions.Interval,
			s.kcpOptions.Resend,
			s.kcpOptions.NoCongestion,
		)
		kcpConn.SetStreamMode(true)
		kcpConn.SetWriteDelay(false)
		kcpConn.SetACKNoDelay(true)

		go s.handleConnection(kcpConn)
	}
}

func (s *Service[U]) handleConnection(kcpConn *kcp.UDPSession) {
	session := &serverSession[U]{
		service:       s,
		ctx:           s.ctx,
		kcpConn:       kcpConn,
		authenticated: false,
		connDone:      make(chan struct{}),
		tcpConnMap:    make(map[uint32]*serverTCPConn[U]),
		udpConnMap:    make(map[uint16]*serverUDPConn[U]),
	}

	defer session.close()

	// Authenticate first
	err := session.authenticate()
	if err != nil {
		s.logger.Error(E.Cause(err, "authenticate"))
		return
	}

	// Start handling messages
	session.handleMessages()
}

type serverSession[U comparable] struct {
	service       *Service[U]
	ctx           context.Context
	kcpConn       *kcp.UDPSession
	authenticated bool
	user          U
	uuid          [16]byte
	connDone      chan struct{}
	closeOnce     sync.Once
	connAccess    sync.Mutex
	tcpConnMap    map[uint32]*serverTCPConn[U]
	udpConnMap    map[uint16]*serverUDPConn[U]
	tcpConnAccess sync.Mutex
	udpConnAccess sync.Mutex
}

func (s *serverSession[U]) authenticate() error {
	// Set authentication timeout
	s.kcpConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer s.kcpConn.SetReadDeadline(time.Time{})

	// Read authentication header
	authData := make([]byte, AuthHeaderSize)
	_, err := io.ReadFull(s.kcpConn, authData)
	if err != nil {
		return E.Cause(err, "read auth header")
	}

	// Verify version
	if authData[0] != Version {
		return E.New("unsupported version: ", authData[0])
	}

	// Verify command
	if authData[1] != CommandAuthenticate {
		return E.New("expected authenticate command")
	}

	// Extract UUID and token
	var uuid [16]byte
	copy(uuid[:], authData[2:18])
	receivedToken := authData[18:50]

	// Lookup user
	s.service.userAccess.RLock()
	user, ok := s.service.userMap[uuid]
	password := s.service.passwordMap[uuid]
	s.service.userAccess.RUnlock()

	if !ok {
		// Send failure response
		s.kcpConn.Write([]byte{1})
		return E.New("user not found")
	}

	// Verify token
	h := sha256.New()
	h.Write(uuid[:])
	h.Write([]byte(password))
	expectedToken := h.Sum(nil)

	if subtle.ConstantTimeCompare(receivedToken, expectedToken) != 1 {
		// Send failure response
		s.kcpConn.Write([]byte{1})
		return E.New("invalid token")
	}

	// Authentication successful
	s.authenticated = true
	s.user = user
	s.uuid = uuid

	// Send success response
	_, err = s.kcpConn.Write([]byte{0})
	if err != nil {
		return E.Cause(err, "send auth response")
	}

	s.service.logger.Info("authenticated user: ", uuid)
	return nil
}

func (s *serverSession[U]) handleMessages() {
	// Start heartbeat handler
	go s.handleHeartbeat()

	for {
		select {
		case <-s.connDone:
			return
		default:
		}

		// Read message header
		header := make([]byte, 2)
		_, err := io.ReadFull(s.kcpConn, header)
		if err != nil {
			if !E.IsClosedOrCanceled(err) {
				s.service.logger.Debug(E.Cause(err, "read header"))
			}
			return
		}

		version := header[0]
		if version != Version {
			s.service.logger.Error("unsupported version: ", version)
			return
		}

		command := header[1]
		switch command {
		case CommandHeartbeat:
			// Respond to heartbeat
			s.connAccess.Lock()
			_, err = s.kcpConn.Write([]byte{Version, CommandHeartbeat})
			s.connAccess.Unlock()
			if err != nil {
				s.service.logger.Debug(E.Cause(err, "send heartbeat"))
				return
			}

		case CommandConnect:
			err = s.handleConnect()

		case CommandPacket:
			err = s.handlePacket()

		case CommandDissociate:
			err = s.handleDissociate()

		default:
			s.service.logger.Error("unknown command: ", command)
			return
		}

		if err != nil {
			if !E.IsClosedOrCanceled(err) {
				s.service.logger.Debug("handle command error: ", err)
			}
			return
		}
	}
}

func (s *serverSession[U]) handleConnect() error {
	// Read connection ID
	connIDBytes := make([]byte, 4)
	_, err := io.ReadFull(s.kcpConn, connIDBytes)
	if err != nil {
		return err
	}
	connID := binary.BigEndian.Uint32(connIDBytes)

	// Read length
	lengthBytes := make([]byte, 2)
	_, err = io.ReadFull(s.kcpConn, lengthBytes)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(lengthBytes)

	// Check if this is a new connection (length > 0 means initial connect with address)
	if length > 0 {
		// Check if it's an address or data
		s.tcpConnAccess.Lock()
		_, exists := s.tcpConnMap[connID]
		s.tcpConnAccess.Unlock()

		if !exists {
			// New connection - read destination address
			destBytes := make([]byte, length)
			_, err = io.ReadFull(s.kcpConn, destBytes)
			if err != nil {
				return err
			}

			destination := M.ParseSocksaddr(string(destBytes))

			// Create TCP connection wrapper
			tcpConn := &serverTCPConn[U]{
				session:   s,
				connID:    connID,
				data:      make(chan *buf.Buffer, 64),
				closeOnce: sync.Once{},
				closeChan: make(chan struct{}),
			}

			s.tcpConnAccess.Lock()
			s.tcpConnMap[connID] = tcpConn
			s.tcpConnAccess.Unlock()

			// Handle connection through handler
			s.service.logger.Debug("new TCP connection to ", destination)
			go func() {
				ctx := auth.ContextWithUser(s.ctx, s.user)
				s.service.handler.NewConnectionEx(ctx, tcpConn, M.SocksaddrFromNet(s.kcpConn.RemoteAddr()).Unwrap(), destination, nil)
				tcpConn.Close()
			}()

			return nil
		}
	}

	// This is data for an existing connection
	if length == 0 {
		return nil
	}

	// Find existing connection
	s.tcpConnAccess.Lock()
	tcpConn, ok := s.tcpConnMap[connID]
	s.tcpConnAccess.Unlock()

	if !ok {
		// Connection not found, skip the data
		io.CopyN(io.Discard, s.kcpConn, int64(length))
		return nil
	}

	// Read data
	dataBuffer := buf.NewSize(int(length))
	_, err = dataBuffer.ReadFullFrom(s.kcpConn, int(length))
	if err != nil {
		dataBuffer.Release()
		return err
	}

	// Deliver data
	select {
	case tcpConn.data <- dataBuffer:
	default:
		dataBuffer.Release()
	}

	return nil
}

func (s *serverSession[U]) handlePacket() error {
	// Read association ID
	assocIDBytes := make([]byte, 2)
	_, err := io.ReadFull(s.kcpConn, assocIDBytes)
	if err != nil {
		return err
	}
	assocID := binary.BigEndian.Uint16(assocIDBytes)

	// Read packet length
	lengthBytes := make([]byte, 2)
	_, err = io.ReadFull(s.kcpConn, lengthBytes)
	if err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(lengthBytes)

	// Read packet data
	packetBuffer := buf.NewSize(int(length))
	_, err = packetBuffer.ReadFullFrom(s.kcpConn, int(length))
	if err != nil {
		packetBuffer.Release()
		return err
	}

	// Get or create UDP connection
	s.udpConnAccess.Lock()
	udpConn, ok := s.udpConnMap[assocID]
	if !ok {
		// Create new UDP association
		udpConn = &serverUDPConn[U]{
			session:   s,
			assocID:   assocID,
			data:      make(chan *udpPacket, 64),
			closeOnce: sync.Once{},
			closeChan: make(chan struct{}),
		}
		s.udpConnMap[assocID] = udpConn

		// Start handling packets
		go s.handleUDPAssociation(udpConn)
	}
	s.udpConnAccess.Unlock()

	// Send packet to handler
	select {
	case udpConn.data <- &udpPacket{buffer: packetBuffer}:
	default:
		packetBuffer.Release()
	}

	return nil
}

func (s *serverSession[U]) handleUDPAssociation(udpConn *serverUDPConn[U]) {
	s.service.logger.Debug("new UDP association")
	ctx := auth.ContextWithUser(s.ctx, s.user)
	s.service.handler.NewPacketConnectionEx(ctx, udpConn, M.SocksaddrFromNet(s.kcpConn.RemoteAddr()).Unwrap(), M.Socksaddr{}, nil)

	// Clean up
	s.udpConnAccess.Lock()
	delete(s.udpConnMap, udpConn.assocID)
	s.udpConnAccess.Unlock()

	udpConn.Close()
}

func (s *serverSession[U]) handleDissociate() error {
	// Read ID (4 bytes for TCP connection ID)
	idBytes := make([]byte, 4)
	_, err := io.ReadFull(s.kcpConn, idBytes)
	if err != nil {
		return err
	}

	// Try TCP first (4 bytes)
	connID := binary.BigEndian.Uint32(idBytes)
	s.tcpConnAccess.Lock()
	tcpConn, ok := s.tcpConnMap[connID]
	if ok {
		delete(s.tcpConnMap, connID)
	}
	s.tcpConnAccess.Unlock()

	if ok {
		tcpConn.closeWithError(io.EOF)
		return nil
	}

	// Try UDP (use first 2 bytes)
	assocID := binary.BigEndian.Uint16(idBytes[:2])
	s.udpConnAccess.Lock()
	udpConn, ok := s.udpConnMap[assocID]
	if ok {
		delete(s.udpConnMap, assocID)
	}
	s.udpConnAccess.Unlock()

	if ok {
		udpConn.closeWithError(io.EOF)
	}

	return nil
}

func (s *serverSession[U]) handleHeartbeat() {
	ticker := time.NewTicker(s.service.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-s.connDone:
			return
		case <-ticker.C:
			// Send heartbeat
			s.connAccess.Lock()
			_, err := s.kcpConn.Write([]byte{Version, CommandHeartbeat})
			s.connAccess.Unlock()

			if err != nil {
				s.service.logger.Debug(E.Cause(err, "send heartbeat"))
				return
			}
		}
	}
}

func (s *serverSession[U]) close() {
	s.closeOnce.Do(func() {
		close(s.connDone)

		// Close all TCP connections
		s.tcpConnAccess.Lock()
		tcpConns := make([]*serverTCPConn[U], 0, len(s.tcpConnMap))
		for _, tcpConn := range s.tcpConnMap {
			tcpConns = append(tcpConns, tcpConn)
		}
		s.tcpConnMap = make(map[uint32]*serverTCPConn[U])
		s.tcpConnAccess.Unlock()

		for _, tcpConn := range tcpConns {
			tcpConn.closeWithError(io.EOF)
		}

		// Close all UDP associations
		s.udpConnAccess.Lock()
		udpConns := make([]*serverUDPConn[U], 0, len(s.udpConnMap))
		for _, udpConn := range s.udpConnMap {
			udpConns = append(udpConns, udpConn)
		}
		s.udpConnMap = make(map[uint16]*serverUDPConn[U])
		s.udpConnAccess.Unlock()

		for _, udpConn := range udpConns {
			udpConn.closeWithError(io.EOF)
		}

		s.kcpConn.Close()
	})
}

type serverTCPConn[U comparable] struct {
	session   *serverSession[U]
	connID    uint32
	data      chan *buf.Buffer
	closeOnce sync.Once
	closeChan chan struct{}
}

func (c *serverTCPConn[U]) Read(p []byte) (n int, err error) {
	select {
	case <-c.closeChan:
		return 0, net.ErrClosed
	case <-c.session.connDone:
		return 0, io.EOF
	case buffer := <-c.data:
		n = copy(p, buffer.Bytes())
		buffer.Release()
		return n, nil
	}
}

func (c *serverTCPConn[U]) Write(p []byte) (n int, err error) {
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

	// Send packet
	c.session.connAccess.Lock()
	_, err = c.session.kcpConn.Write(packet.Bytes())
	c.session.connAccess.Unlock()

	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *serverTCPConn[U]) Close() error {
	c.closeWithError(nil)
	return nil
}

func (c *serverTCPConn[U]) closeWithError(err error) {
	c.closeOnce.Do(func() {
		close(c.closeChan)

		// Remove from session
		c.session.tcpConnAccess.Lock()
		delete(c.session.tcpConnMap, c.connID)
		c.session.tcpConnAccess.Unlock()

		// Send dissociate
		dissociate := buf.NewSize(2 + 4)
		dissociate.WriteByte(Version)
		dissociate.WriteByte(CommandDissociate)
		binary.Write(dissociate, binary.BigEndian, c.connID)

		c.session.connAccess.Lock()
		c.session.kcpConn.Write(dissociate.Bytes())
		c.session.connAccess.Unlock()

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

func (c *serverTCPConn[U]) LocalAddr() net.Addr {
	return c.session.kcpConn.LocalAddr()
}

func (c *serverTCPConn[U]) RemoteAddr() net.Addr {
	return c.session.kcpConn.RemoteAddr()
}

func (c *serverTCPConn[U]) SetDeadline(t time.Time) error {
	return nil
}

func (c *serverTCPConn[U]) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *serverTCPConn[U]) SetWriteDeadline(t time.Time) error {
	return nil
}

type serverUDPConn[U comparable] struct {
	session   *serverSession[U]
	assocID   uint16
	data      chan *udpPacket
	closeOnce sync.Once
	closeChan chan struct{}
}

type udpPacket struct {
	buffer      *buf.Buffer
	destination M.Socksaddr
}

func (c *serverUDPConn[U]) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	select {
	case <-c.closeChan:
		return M.Socksaddr{}, io.EOF
	case <-c.session.connDone:
		return M.Socksaddr{}, io.EOF
	case packet := <-c.data:
		_, err = buffer.Write(packet.buffer.Bytes())
		destination = packet.destination
		packet.buffer.Release()
		return
	}
}

func (c *serverUDPConn[U]) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	select {
	case <-c.closeChan:
		return net.ErrClosed
	default:
	}

	// Build packet
	packet := buf.NewSize(PacketHeaderSize + 2 + buffer.Len())
	defer packet.Release()

	packet.WriteByte(Version)
	packet.WriteByte(CommandPacket)
	binary.Write(packet, binary.BigEndian, c.assocID)
	binary.Write(packet, binary.BigEndian, uint16(buffer.Len()))
	packet.Write(buffer.Bytes())

	// Send packet
	c.session.connAccess.Lock()
	_, err := c.session.kcpConn.Write(packet.Bytes())
	c.session.connAccess.Unlock()

	return err
}

func (c *serverUDPConn[U]) Close() error {
	c.closeWithError(nil)
	return nil
}

func (c *serverUDPConn[U]) closeWithError(err error) {
	c.closeOnce.Do(func() {
		close(c.closeChan)

		// Drain data channel
		for {
			select {
			case packet := <-c.data:
				packet.buffer.Release()
			default:
				return
			}
		}
	})
}

func (c *serverUDPConn[U]) LocalAddr() net.Addr {
	return c.session.kcpConn.LocalAddr()
}

func (c *serverUDPConn[U]) SetDeadline(t time.Time) error {
	return nil
}

func (c *serverUDPConn[U]) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *serverUDPConn[U]) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *serverUDPConn[U]) Upstream() any {
	return c.session.kcpConn
}
