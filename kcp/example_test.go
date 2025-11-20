package kcp

import (
	"context"
	"crypto/sha256"
	"net"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// mockHandler implements ServerHandler for testing
type mockHandler struct{}

func (h *mockHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, closeHandler N.CloseHandlerFunc) {
	// Echo server for testing
	go func() {
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			_, err = conn.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
}

func (h *mockHandler) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, closeHandler N.CloseHandlerFunc) {
	// Echo server for UDP
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, addr, err := conn.(net.PacketConn).ReadFrom(buffer)
			if err != nil {
				return
			}
			_, err = conn.(net.PacketConn).WriteTo(buffer[:n], addr)
			if err != nil {
				return
			}
		}
	}()
}

// mockDialer implements N.Dialer for testing
type mockDialer struct{}

func (d *mockDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return net.Dial(network, destination.String())
}

func (d *mockDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return net.ListenPacket("udp", "")
}

// TestKCPOptions tests different KCP configuration presets
func TestKCPOptions(t *testing.T) {
	tests := []struct {
		name    string
		options KCPOptions
	}{
		{
			name:    "Default",
			options: DefaultKCPOptions(),
		},
		{
			name:    "Conservative",
			options: ConservativeKCPOptions(),
		},
		{
			name:    "Aggressive",
			options: AggressiveKCPOptions(),
		},
		{
			name: "Custom",
			options: KCPOptions{
				MTU:          1350,
				SndWnd:       256,
				RcvWnd:       1024,
				NoDelay:      1,
				Interval:     15,
				Resend:       2,
				NoCongestion: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate configuration
			if tt.options.MTU <= 0 {
				t.Errorf("Invalid MTU: %d", tt.options.MTU)
			}
			if tt.options.SndWnd <= 0 {
				t.Errorf("Invalid SndWnd: %d", tt.options.SndWnd)
			}
			if tt.options.RcvWnd <= 0 {
				t.Errorf("Invalid RcvWnd: %d", tt.options.RcvWnd)
			}
			if tt.options.Interval < 0 {
				t.Errorf("Invalid Interval: %d", tt.options.Interval)
			}
			if tt.options.NoDelay < 0 || tt.options.NoDelay > 1 {
				t.Errorf("Invalid NoDelay: %d", tt.options.NoDelay)
			}
			if tt.options.NoCongestion < 0 || tt.options.NoCongestion > 1 {
				t.Errorf("Invalid NoCongestion: %d", tt.options.NoCongestion)
			}
		})
	}
}

// TestClientCreation tests client creation with different options
func TestClientCreation(t *testing.T) {
	testUUID := uuid.Must(uuid.NewV4())
	var uuidBytes [16]byte
	copy(uuidBytes[:], testUUID.Bytes())

	tests := []struct {
		name    string
		options ClientOptions
		wantErr bool
	}{
		{
			name: "Valid default options",
			options: ClientOptions{
				Context:       context.Background(),
				Dialer:        &mockDialer{},
				ServerAddress: M.ParseSocksaddr("127.0.0.1:8388"),
				UUID:          uuidBytes,
				Password:      "test-password",
				KCPOptions:    DefaultKCPOptions(),
				Crypt:         "test-crypt-key",
				Heartbeat:     10 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "With FEC",
			options: ClientOptions{
				Context:       context.Background(),
				Dialer:        &mockDialer{},
				ServerAddress: M.ParseSocksaddr("127.0.0.1:8388"),
				UUID:          uuidBytes,
				Password:      "test-password",
				KCPOptions:    DefaultKCPOptions(),
				Crypt:         "test-crypt-key",
				DataShard:     10,
				ParityShard:   3,
				Heartbeat:     10 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "No encryption",
			options: ClientOptions{
				Context:       context.Background(),
				Dialer:        &mockDialer{},
				ServerAddress: M.ParseSocksaddr("127.0.0.1:8388"),
				UUID:          uuidBytes,
				Password:      "test-password",
				KCPOptions:    DefaultKCPOptions(),
				Crypt:         "",
				Heartbeat:     10 * time.Second,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.options)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewClient() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if client == nil && !tt.wantErr {
				t.Error("NewClient() returned nil client")
			}
		})
	}
}

// TestServiceCreation tests service creation with different options
func TestServiceCreation(t *testing.T) {
	tests := []struct {
		name    string
		options ServiceOptions
		wantErr bool
	}{
		{
			name: "Valid default options",
			options: ServiceOptions{
				Context:     context.Background(),
				Logger:      logger.NOP(),
				KCPOptions:  DefaultKCPOptions(),
				Crypt:       "test-crypt-key",
				Heartbeat:   10 * time.Second,
				Handler:     &mockHandler{},
			},
			wantErr: false,
		},
		{
			name: "With FEC",
			options: ServiceOptions{
				Context:     context.Background(),
				Logger:      logger.NOP(),
				KCPOptions:  DefaultKCPOptions(),
				Crypt:       "test-crypt-key",
				DataShard:   10,
				ParityShard: 3,
				Heartbeat:   10 * time.Second,
				Handler:     &mockHandler{},
			},
			wantErr: false,
		},
		{
			name: "Conservative options",
			options: ServiceOptions{
				Context:     context.Background(),
				Logger:      logger.NOP(),
				KCPOptions:  ConservativeKCPOptions(),
				Crypt:       "test-crypt-key",
				DataShard:   10,
				ParityShard: 3,
				Heartbeat:   10 * time.Second,
				Handler:     &mockHandler{},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, err := NewService[string](tt.options)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewService() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if service == nil && !tt.wantErr {
				t.Error("NewService() returned nil service")
			}
		})
	}
}

// TestEncryption tests the encryption key generation
func TestEncryption(t *testing.T) {
	tests := []string{
		"short-key",
		"medium-length-encryption-key",
		"this-is-a-very-long-encryption-key-that-exceeds-normal-length",
		"",
	}

	for _, crypt := range tests {
		t.Run("crypt="+crypt, func(t *testing.T) {
			if crypt == "" {
				// Empty crypt means no encryption, which is valid
				return
			}

			// Test key generation (same as in client)
			key := sha256.Sum256([]byte(crypt))
			if len(key) != 32 {
				t.Errorf("Expected key length 32, got %d", len(key))
			}

			// Verify we can use first 16 bytes for AES
			aesKey := key[:16]
			if len(aesKey) != 16 {
				t.Errorf("Expected AES key length 16, got %d", len(aesKey))
			}
		})
	}
}

// ExampleClient demonstrates how to create and use a KCP client
func ExampleClient() {
	// Parse UUID
	testUUID := uuid.Must(uuid.FromString("12345678-1234-1234-1234-123456789abc"))
	var uuidBytes [16]byte
	copy(uuidBytes[:], testUUID.Bytes())

	// Create client
	client, err := NewClient(ClientOptions{
		Context:       context.Background(),
		Dialer:        &mockDialer{},
		ServerAddress: M.ParseSocksaddr("server.example.com:8388"),
		UUID:          uuidBytes,
		Password:      "my-password",
		KCPOptions:    DefaultKCPOptions(),
		Crypt:         "my-encryption-key",
		DataShard:     10,
		ParityShard:   3,
		Heartbeat:     10 * time.Second,
	})
	if err != nil {
		panic(err)
	}

	// Create TCP connection
	conn, err := client.DialConn(context.Background(), M.ParseSocksaddr("google.com:443"))
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	// Use conn like normal net.Conn
	conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
}

// ExampleService demonstrates how to create and use a KCP service
func ExampleService() {
	// Parse UUID
	testUUID := uuid.Must(uuid.FromString("12345678-1234-1234-1234-123456789abc"))
	var uuidBytes [16]byte
	copy(uuidBytes[:], testUUID.Bytes())

	// Create service
	service, err := NewService[string](ServiceOptions{
		Context:     context.Background(),
		Logger:      logger.NOP(),
		KCPOptions:  DefaultKCPOptions(),
		Crypt:       "my-encryption-key",
		DataShard:   10,
		ParityShard: 3,
		Heartbeat:   10 * time.Second,
		Handler:     &mockHandler{},
	})
	if err != nil {
		panic(err)
	}

	// Update users
	service.UpdateUsers(
		[]string{"user1"},
		[][16]byte{uuidBytes},
		[]string{"my-password"},
	)

	// Start listening
	udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:8388")
	udpConn, _ := net.ListenUDP("udp", udpAddr)

	err = service.Start(udpConn)
	if err != nil {
		panic(err)
	}

	// Service runs in background
}
