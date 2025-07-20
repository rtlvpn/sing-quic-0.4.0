package tuic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/sagernet/quic-go"
	qtls "github.com/sagernet/sing-quic"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/baderror"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	aTLS "github.com/sagernet/sing/common/tls"

	"github.com/gofrs/uuid/v5"
)

type ServiceOptions struct {
	Context           context.Context
	Logger            logger.Logger
	TLSConfig         aTLS.ServerConfig
	CongestionControl string
	AuthTimeout       time.Duration
	ZeroRTTHandshake  bool
	Heartbeat         time.Duration
	UDPTimeout        time.Duration
	Handler           ServiceHandler
}

type ServiceHandler interface {
	N.TCPConnectionHandlerEx
	N.UDPConnectionHandlerEx
}

type Service[U comparable] struct {
	ctx               context.Context
	logger            logger.Logger
	tlsConfig         aTLS.ServerConfig
	heartbeat         time.Duration
	quicConfig        *quic.Config
	userMap           map[[16]byte]U
	passwordMap       map[U]string
	congestionControl string
	authTimeout       time.Duration
	udpTimeout        time.Duration
	handler           ServiceHandler

	quicListener io.Closer
}

func NewService[U comparable](options ServiceOptions) (*Service[U], error) {
	// Initialize random number generator for proper randomness
	rand.Seed(time.Now().UnixNano())

	if options.AuthTimeout == 0 {
		options.AuthTimeout = 3 * time.Second
	}
	if options.Heartbeat == 0 {
		options.Heartbeat = 10 * time.Second
	}
	quicConfig := &quic.Config{
		DisablePathMTUDiscovery: !(runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "android" || runtime.GOOS == "darwin"),
		EnableDatagrams:         true,
		Allow0RTT:               options.ZeroRTTHandshake,
		MaxIncomingStreams:      1 << 60,
		MaxIncomingUniStreams:   1 << 60,
	}
	switch options.CongestionControl {
	case "":
		options.CongestionControl = "cubic"
	case "cubic", "new_reno", "bbr", "brutal":
	default:
		return nil, E.New("unknown congestion control algorithm: ", options.CongestionControl)
	}
	return &Service[U]{
		ctx:               options.Context,
		logger:            options.Logger,
		tlsConfig:         options.TLSConfig,
		heartbeat:         options.Heartbeat,
		quicConfig:        quicConfig,
		userMap:           make(map[[16]byte]U),
		congestionControl: options.CongestionControl,
		authTimeout:       options.AuthTimeout,
		udpTimeout:        options.UDPTimeout,
		handler:           options.Handler,
	}, nil
}

func (s *Service[U]) UpdateUsers(userList []U, uuidList [][16]byte, passwordList []string) {
	userMap := make(map[[16]byte]U)
	passwordMap := make(map[U]string)
	for index := range userList {
		userMap[uuidList[index]] = userList[index]
		passwordMap[userList[index]] = passwordList[index]
	}
	s.userMap = userMap
	s.passwordMap = passwordMap
}

func (s *Service[U]) Start(conn net.PacketConn) error {
	// Wrap the provided connection with our custom packet connection
	// that intercepts and handles fake Steam packets
	wrappedConn := &fakeSteamInterceptor{
		PacketConn: conn,
		service:    s,
	}

	if !s.quicConfig.Allow0RTT {
		listener, err := qtls.Listen(wrappedConn, s.tlsConfig, s.quicConfig)
		if err != nil {
			return err
		}
		s.quicListener = listener
		go func() {
			for {
				connection, hErr := listener.Accept(s.ctx)
				if hErr != nil {
					if E.IsClosedOrCanceled(hErr) || errors.Is(hErr, quic.ErrServerClosed) {
						s.logger.Debug(E.Cause(hErr, "listener closed"))
					} else {
						s.logger.Error(E.Cause(hErr, "listener closed"))
					}
					return
				}
				go s.handleConnection(connection)
			}
		}()
	} else {
		listener, err := qtls.ListenEarly(wrappedConn, s.tlsConfig, s.quicConfig)
		if err != nil {
			return err
		}
		s.quicListener = listener
		go func() {
			for {
				connection, hErr := listener.Accept(s.ctx)
				if hErr != nil {
					if E.IsClosedOrCanceled(hErr) || errors.Is(hErr, quic.ErrServerClosed) {
						s.logger.Debug(E.Cause(hErr, "listener closed"))
					} else {
						s.logger.Error(E.Cause(hErr, "listener closed"))
					}
					return
				}
				go s.handleConnection(connection)
			}
		}()
	}
	return nil
}

func (s *Service[U]) Close() error {
	return common.Close(
		s.quicListener,
	)
}

func (s *Service[U]) handleConnection(connection quic.Connection) {
	setCongestion(s.ctx, connection, s.congestionControl)
	session := &serverSession[U]{
		Service:    s,
		ctx:        s.ctx,
		quicConn:   connection,
		source:     M.SocksaddrFromNet(connection.RemoteAddr()).Unwrap(),
		connDone:   make(chan struct{}),
		authDone:   make(chan struct{}),
		udpConnMap: make(map[uint16]*udpPacketConn),
	}
	session.handle()
}

// handleFakeSteamPacket processes incoming fake Steam packets and responds with appropriate server responses
// This is used to mimic Steam server behavior before establishing the QUIC connection
func (s *Service[U]) handleFakeSteamPacket(conn net.PacketConn, remoteAddr net.Addr, data []byte) {
	// Check if this looks like a Steam client packet
	if len(data) < 8 {
		return // Too short to be a Steam packet
	}

	// Check for Steam packet header (0xFF, 0xFF, 0xFF, 0xFF)
	if data[0] == 0xFF && data[1] == 0xFF && data[2] == 0xFF && data[3] == 0xFF {
		// Determine packet type
		if len(data) >= 12 && data[4] == 0x71 && data[5] == 0x30 && data[6] == 0x30 && data[7] == 0x30 {
			// This is a heartbeat packet, respond with a server heartbeat acknowledgment
			// Extract sequence number
			seqNum := binary.LittleEndian.Uint32(data[8:12])

			// Create server response
			serverResponse := []byte{
				0xFF, 0xFF, 0xFF, 0xFF, // Steam packet header
				0x72, 0x30, 0x30, 0x30, // Server heartbeat response prefix
				0x00, 0x00, 0x00, 0x00, // Sequence number (will be set below)
				// Server payload
				0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
				0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8,
			}

			// Set the sequence number in the response
			binary.LittleEndian.PutUint32(serverResponse[8:12], seqNum)

			// Add a small delay before responding (150-250ms)
			delayMs := 150 + rand.Intn(100)
			time.Sleep(time.Duration(delayMs) * time.Millisecond)

			// Send the response
			conn.WriteTo(serverResponse, remoteAddr)

		} else if len(data) >= 16 && data[4] == 0x56 && data[5] == 0x41 && data[6] == 0x4c && data[7] == 0x56 {
			// This is a game data packet (VALV header), respond with a server game data packet

			// Create server game data response
			serverGameData := []byte{
				0xFF, 0xFF, 0xFF, 0xFF, // Steam packet header
				0x56, 0x41, 0x4c, 0x53, // "VALS" (Server variant of VALV)
				0x45, 0x52, 0x56, 0x45, // "ERVE"
				0x52, 0x20, 0x30, 0x31, // "R 01"
				// Server game data payload
				0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8,
				0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7, 0xC8,
				0xD1, 0xD2, 0xD3, 0xD4, 0xD5, 0xD6, 0xD7, 0xD8,
				0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7, 0xE8,
			}

			// Copy some bytes from the request to make the response look related
			if len(data) >= 20 {
				copy(serverGameData[16:20], data[16:20])
			}

			// Add a variable delay before responding (200-350ms)
			delayMs := 200 + rand.Intn(150)
			time.Sleep(time.Duration(delayMs) * time.Millisecond)

			// Send the response
			conn.WriteTo(serverGameData, remoteAddr)
		}
	}
}

type serverSession[U comparable] struct {
	*Service[U]
	ctx        context.Context
	quicConn   quic.Connection
	source     M.Socksaddr
	connAccess sync.Mutex
	connDone   chan struct{}
	connErr    error
	authDone   chan struct{}
	authUser   U
	udpAccess  sync.RWMutex
	udpConnMap map[uint16]*udpPacketConn
}

func (s *serverSession[U]) handle() {
	if s.ctx.Done() != nil {
		go func() {
			select {
			case <-s.ctx.Done():
				s.closeWithError(s.ctx.Err())
			case <-s.connDone:
			}
		}()
	}
	go s.loopUniStreams()
	go s.loopStreams()
	go s.loopMessages()
	go s.handleAuthTimeout()
	go s.loopHeartbeats()
}

func (s *serverSession[U]) loopUniStreams() {
	for {
		uniStream, err := s.quicConn.AcceptUniStream(s.ctx)
		if err != nil {
			return
		}
		go func() {
			err = s.handleUniStream(uniStream)
			if err != nil {
				s.closeWithError(E.Cause(err, "handle uni stream"))
			}
		}()
	}
}

func (s *serverSession[U]) handleUniStream(stream quic.ReceiveStream) error {
	defer stream.CancelRead(0)
	buffer := buf.New()
	defer buffer.Release()
	_, err := buffer.ReadAtLeastFrom(stream, 2)
	if err != nil {
		return E.Cause(err, "read request")
	}
	version := buffer.Byte(0)
	if version != Version {
		return E.New("unknown version ", buffer.Byte(0))
	}
	command := buffer.Byte(1)
	switch command {
	case CommandAuthenticate:
		select {
		case <-s.authDone:
			return E.New("authentication: multiple authentication requests")
		default:
		}
		if buffer.Len() < AuthenticateLen {
			_, err = buffer.ReadFullFrom(stream, AuthenticateLen-buffer.Len())
			if err != nil {
				return E.Cause(err, "authentication: read request")
			}
		}
		var userUUID [16]byte
		copy(userUUID[:], buffer.Range(2, 2+16))
		user, loaded := s.userMap[userUUID]
		if !loaded {
			return E.New("authentication: unknown user ", uuid.UUID(userUUID))
		}
		handshakeState := s.quicConn.ConnectionState()
		tuicToken, err := handshakeState.ExportKeyingMaterial(string(userUUID[:]), []byte(s.passwordMap[user]), 32)
		if err != nil {
			return E.Cause(err, "authentication: export keying material")
		}
		if !bytes.Equal(tuicToken, buffer.Range(2+16, 2+16+32)) {
			return E.New("authentication: token mismatch")
		}
		s.authUser = user
		close(s.authDone)
		return nil
	case CommandPacket:
		select {
		case <-s.connDone:
			return s.connErr
		case <-s.authDone:
		}
		message := allocMessage()
		err = readUDPMessage(message, io.MultiReader(bytes.NewReader(buffer.From(2)), stream))
		if err != nil {
			message.release()
			return err
		}
		s.handleUDPMessage(message, true)
		return nil
	case CommandDissociate:
		select {
		case <-s.connDone:
			return s.connErr
		case <-s.authDone:
		}
		if buffer.Len() > 4 {
			return E.New("invalid dissociate message")
		}
		var sessionID uint16
		err = binary.Read(io.MultiReader(bytes.NewReader(buffer.From(2)), stream), binary.BigEndian, &sessionID)
		if err != nil {
			return err
		}
		s.udpAccess.RLock()
		udpConn, loaded := s.udpConnMap[sessionID]
		s.udpAccess.RUnlock()
		if loaded {
			udpConn.closeWithError(E.New("remote closed"))
			s.udpAccess.Lock()
			delete(s.udpConnMap, sessionID)
			s.udpAccess.Unlock()
		}
		return nil
	default:
		return E.New("unknown command ", command)
	}
}

func (s *serverSession[U]) handleAuthTimeout() {
	select {
	case <-s.connDone:
	case <-s.authDone:
	case <-time.After(s.authTimeout):
		s.closeWithError(E.New("authentication timeout"))
	}
}

func (s *serverSession[U]) loopStreams() {
	for {
		stream, err := s.quicConn.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		go func() {
			err = s.handleStream(stream)
			if err != nil {
				stream.CancelRead(0)
				stream.Close()
				s.logger.Error(E.Cause(err, "handle stream request"))
			}
		}()
	}
}

func (s *serverSession[U]) handleStream(stream quic.Stream) error {
	buffer := buf.NewSize(2 + M.MaxSocksaddrLength)
	defer buffer.Release()
	_, err := buffer.ReadAtLeastFrom(stream, 2)
	if err != nil {
		return E.Cause(err, "read request")
	}
	version, _ := buffer.ReadByte()
	if version != Version {
		return E.New("unknown version ", buffer.Byte(0))
	}
	command, _ := buffer.ReadByte()
	if command != CommandConnect {
		return E.New("unsupported stream command ", command)
	}
	destination, err := AddressSerializer.ReadAddrPort(io.MultiReader(buffer, stream))
	if err != nil {
		return E.Cause(err, "read request destination")
	}
	select {
	case <-s.connDone:
		return s.connErr
	case <-s.authDone:
	}
	var conn net.Conn = &serverConn{
		Stream:      stream,
		destination: destination,
	}
	if buffer.IsEmpty() {
		buffer.Release()
	} else {
		conn = bufio.NewCachedConn(conn, buffer)
	}
	s.handler.NewConnectionEx(auth.ContextWithUser(s.ctx, s.authUser), conn, s.source, destination, nil)
	return nil
}

func (s *serverSession[U]) loopHeartbeats() {
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.connDone:
			return
		case <-ticker.C:
			err := s.quicConn.SendDatagram([]byte{Version, CommandHeartbeat})
			if err != nil {
				s.closeWithError(E.Cause(err, "send heartbeat"))
			}
		}
	}
}

func (s *serverSession[U]) closeWithError(err error) {
	s.connAccess.Lock()
	defer s.connAccess.Unlock()
	select {
	case <-s.connDone:
		return
	default:
		s.connErr = err
		close(s.connDone)
	}
	if E.IsClosedOrCanceled(err) {
		s.logger.Debug(E.Cause(err, "connection failed"))
	} else {
		s.logger.Error(E.Cause(err, "connection failed"))
	}
	_ = s.quicConn.CloseWithError(0, "")
}

type serverConn struct {
	quic.Stream
	destination M.Socksaddr
}

func (c *serverConn) Read(p []byte) (n int, err error) {
	n, err = c.Stream.Read(p)
	return n, baderror.WrapQUIC(err)
}

func (c *serverConn) Write(p []byte) (n int, err error) {
	n, err = c.Stream.Write(p)
	return n, baderror.WrapQUIC(err)
}

func (c *serverConn) LocalAddr() net.Addr {
	return c.destination
}

func (c *serverConn) RemoteAddr() net.Addr {
	return M.Socksaddr{}
}

func (c *serverConn) Close() error {
	c.Stream.CancelRead(0)
	return c.Stream.Close()
}

// fakeSteamInterceptor is a wrapper around a net.PacketConn that intercepts
// and handles fake Steam packets before passing other packets to the QUIC handler
type fakeSteamInterceptor struct {
	net.PacketConn
	service interface {
		handleFakeSteamPacket(conn net.PacketConn, remoteAddr net.Addr, data []byte)
	}
}

// ReadFrom intercepts packets, processes fake Steam packets, and passes other packets to QUIC
func (f *fakeSteamInterceptor) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, addr, err = f.PacketConn.ReadFrom(p)
	if err != nil {
		return
	}

	// Check if this might be a fake Steam packet (they start with 0xFF 0xFF 0xFF 0xFF)
	if n >= 4 && p[0] == 0xFF && p[1] == 0xFF && p[2] == 0xFF && p[3] == 0xFF {
		// Make a copy of the data for handling separately
		dataCopy := make([]byte, n)
		copy(dataCopy, p[:n])

		// Process the fake Steam packet in a separate goroutine to avoid blocking
		go f.service.handleFakeSteamPacket(f.PacketConn, addr, dataCopy)

		// Return an error to tell QUIC to ignore this packet
		// We use io.EOF which is a sentinel error that QUIC will handle gracefully
		return 0, addr, io.EOF
	}

	// For non-Steam packets, return normally to let QUIC process them
	return
}
