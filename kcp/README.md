# KCP Protocol for sing-box

A clean KCP implementation integrated with sing-quic and sing-box, providing reliable UDP transport with low latency.

## Overview

KCP is a fast and reliable ARQ protocol that achieves significantly lower latency than TCP while maintaining reliability. This implementation properly integrates with sing-box's architecture using clean layer separation.

## Architecture

Unlike the previous broken implementation that mixed KCP-GO's native listener with sing-box patterns, this version follows proper architectural layers:

### Layer Separation

1. **Transport Layer**: Raw UDP + KCP reliability (via kcp-go)
2. **Encryption Layer**: Optional AES encryption at KCP level
3. **Authentication Layer**: UUID-based authentication with SHA256 token verification
4. **Multiplexing Layer**: 
   - TCP-like connections via KCP streams
   - UDP associations with packet forwarding
5. **Application Layer**: Standard `net.Conn` and `N.PacketConn` interfaces

### Key Differences from Old Implementation

**Problems in Old Code:**
- Mixed KCP-GO's listener with sing-box dialer patterns
- Confused authentication/encryption/transport layers
- Tried to manually multiplex over raw KCP sessions
- Not compatible with sing-box's `N.Dialer` and handler interfaces

**New Architecture:**
- Uses KCP's built-in stream multiplexing (`OpenStream`/`AcceptStream`)
- Proper authentication after KCP session establishment
- Clean separation: UDP → KCP → Auth → Streams/Packets → Application
- Fully compatible with sing-box patterns (like Hysteria2/TUIC)

## Configuration

### Client Options

```go
type ClientOptions struct {
    Context       context.Context
    Dialer        N.Dialer          // UDP dialer
    ServerAddress M.Socksaddr       // Server address
    UUID          [16]byte          // User UUID
    Password      string            // User password
    KCPOptions    KCPOptions        // KCP tuning parameters
    Crypt         string            // Encryption key (optional)
    DataShard     int               // FEC data shards (optional)
    ParityShard   int               // FEC parity shards (optional)
    Heartbeat     time.Duration     // Heartbeat interval
}
```

### Server Options

```go
type ServiceOptions struct {
    Context     context.Context
    Logger      logger.Logger
    KCPOptions  KCPOptions
    Crypt       string            // Must match client
    DataShard   int               // Must match client
    ParityShard int               // Must match client
    Heartbeat   time.Duration
    Handler     ServerHandler     // Connection handler
}
```

### KCP Tuning Options

```go
type KCPOptions struct {
    MTU          int   // Maximum Transmission Unit (default: 1350)
    SndWnd       int   // Send window size (default: 128)
    RcvWnd       int   // Receive window size (default: 512)
    NoDelay      int   // 0=normal, 1=no delay (default: 1)
    Interval     int   // Update interval in ms (default: 20)
    Resend       int   // Fast resend mode (default: 2)
    NoCongestion int   // 0=normal, 1=disabled (default: 1)
}
```

### Preset Configurations

```go
// Default: Balanced performance and reliability
opts := kcp.DefaultKCPOptions()

// Conservative: Better for unstable networks
opts := kcp.ConservativeKCPOptions()

// Aggressive: Maximum speed on good networks  
opts := kcp.AggressiveKCPOptions()
```

## Usage Examples

### Client

```go
package main

import (
    "context"
    "github.com/sagernet/sing-quic/kcp"
    M "github.com/sagernet/sing/common/metadata"
    N "github.com/sagernet/sing/common/network"
)

func main() {
    // Parse UUID
    uuid, _ := uuid.Parse("12345678-1234-1234-1234-123456789abc")
    
    client, err := kcp.NewClient(kcp.ClientOptions{
        Context:       context.Background(),
        Dialer:        dialer, // Your N.Dialer implementation
        ServerAddress: M.ParseSocksaddr("server.example.com:8388"),
        UUID:          uuid,
        Password:      "mypassword",
        KCPOptions:    kcp.DefaultKCPOptions(),
        Crypt:         "my-encryption-key",
        DataShard:     10,  // Optional FEC
        ParityShard:   3,   // Optional FEC
    })
    if err != nil {
        panic(err)
    }
    
    // Create TCP connection
    conn, err := client.DialConn(context.Background(), 
        M.ParseSocksaddr("google.com:443"))
    if err != nil {
        panic(err)
    }
    defer conn.Close()
    
    // Use conn like normal net.Conn
    conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
    
    // Create UDP association
    packetConn, err := client.ListenPacket(context.Background(),
        M.ParseSocksaddr("8.8.8.8:53"))
    if err != nil {
        panic(err)
    }
    defer packetConn.Close()
    
    // Use packetConn like normal net.PacketConn
}
```

### Server

```go
package main

import (
    "context"
    "net"
    "github.com/sagernet/sing-quic/kcp"
    "github.com/sagernet/sing/common/logger"
)

func main() {
    // Create service
    service, err := kcp.NewService[string](kcp.ServiceOptions{
        Context:    context.Background(),
        Logger:     logger.NewLogger("kcp"),
        KCPOptions: kcp.DefaultKCPOptions(),
        Crypt:      "my-encryption-key",
        DataShard:  10,  // Must match client
        ParityShard: 3,  // Must match client
        Handler:    yourHandler, // Implements ServerHandler
    })
    if err != nil {
        panic(err)
    }
    
    // Update users
    uuid1, _ := uuid.Parse("12345678-1234-1234-1234-123456789abc")
    service.UpdateUsers(
        []string{"user1"},
        [][16]byte{uuid1},
        []string{"mypassword"},
    )
    
    // Start listening on UDP
    udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:8388")
    udpConn, _ := net.ListenUDP("udp", udpAddr)
    
    err = service.Start(udpConn)
    if err != nil {
        panic(err)
    }
    
    // Server runs in background
    select {}
}
```

## sing-box Integration

### Outbound Configuration (Client)

```json
{
  "type": "kcp",
  "tag": "kcp-out",
  "server": "server.example.com",
  "server_port": 8388,
  "uuid": "12345678-1234-1234-1234-123456789abc",
  "password": "mypassword",
  "kcp": {
    "mtu": 1350,
    "snd_wnd": 128,
    "rcv_wnd": 512,
    "nodelay": 1,
    "interval": 20,
    "resend": 2,
    "nc": 1
  },
  "crypt": "my-encryption-key",
  "fec": {
    "data_shard": 10,
    "parity_shard": 3
  }
}
```

### Inbound Configuration (Server)

```json
{
  "type": "kcp",
  "tag": "kcp-in",
  "listen": "0.0.0.0",
  "listen_port": 8388,
  "users": [
    {
      "uuid": "12345678-1234-1234-1234-123456789abc",
      "password": "mypassword"
    }
  ],
  "kcp": {
    "mtu": 1350,
    "snd_wnd": 128,
    "rcv_wnd": 512,
    "nodelay": 1,
    "interval": 20,
    "resend": 2,
    "nc": 1
  },
  "crypt": "my-encryption-key",
  "fec": {
    "data_shard": 10,
    "parity_shard": 3
  }
}
```

## Protocol Design

### Wire Protocol

All multi-byte integers are in big-endian format.

#### Authentication Flow

**Client → Server: Authentication Request**
```
+----------+----------+----------------+------------------+
| Version  | Command  |      UUID      |      Token       |
| (1 byte) | (1 byte) |   (16 bytes)   |   (32 bytes)     |
+----------+----------+----------------+------------------+
```

- Version: Protocol version (currently 1)
- Command: `CommandAuthenticate` (0)
- UUID: User identifier
- Token: SHA256(UUID + Password)

**Server → Client: Authentication Response**
```
+----------+
|  Status  |
| (1 byte) |
+----------+
```

- Status: 0=success, 1=failure

#### TCP Connection (Stream)

After authentication, client opens a KCP stream and sends:

```
+----------+----------+----------+-----------+-------------+
| Version  | Command  | AssocID  |  AddrLen  |   Address   |
| (1 byte) | (1 byte) | (4 bytes)| (2 bytes) |  (variable) |
+----------+----------+----------+-----------+-------------+
```

- Command: `CommandConnect` (1)
- AssocID: Stream association ID
- AddrLen: Length of destination address string
- Address: Destination address (e.g., "google.com:443")

Data then flows bidirectionally over the KCP stream.

#### UDP Packet

```
+----------+----------+----------+-----------+-------------+
| Version  | Command  | AssocID  | DataLen   |    Data     |
| (1 byte) | (1 byte) | (2 bytes)| (2 bytes) |  (variable) |
+----------+----------+----------+-----------+-------------+
```

- Command: `CommandPacket` (2)
- AssocID: UDP association ID
- DataLen: Packet data length
- Data: UDP packet payload

#### Dissociate

```
+----------+----------+----------+
| Version  | Command  | AssocID  |
| (1 byte) | (1 byte) | (2 bytes)|
+----------+----------+----------+
```

- Command: `CommandDissociate` (3)
- AssocID: Association to close

#### Heartbeat

```
+----------+----------+
| Version  | Command  |
| (1 byte) | (1 byte) |
+----------+----------+
```

- Command: `CommandHeartbeat` (4)

## Performance Tuning

### Network Conditions

**Good Network (Low Latency, Low Loss)**
```go
KCPOptions{
    MTU:          1400,
    SndWnd:       512,
    RcvWnd:       2048,
    NoDelay:      1,
    Interval:     10,
    Resend:       2,
    NoCongestion: 1,
}
```

**Unstable Network (High Latency, Packet Loss)**
```go
KCPOptions{
    MTU:          1350,
    SndWnd:       256,
    RcvWnd:       1024,
    NoDelay:      0,
    Interval:     40,
    Resend:       1,
    NoCongestion: 0,
}
// Enable FEC
DataShard:   10,
ParityShard: 3,
```

**Gaming (Lowest Latency)**
```go
KCPOptions{
    MTU:          1350,
    SndWnd:       128,
    RcvWnd:       512,
    NoDelay:      1,
    Interval:     10,
    Resend:       2,
    NoCongestion: 1,
}
```

### Forward Error Correction (FEC)

FEC adds redundancy to recover from packet loss without retransmission:

- `DataShard`: Number of data packets in each FEC group (e.g., 10)
- `ParityShard`: Number of parity packets (e.g., 3)

With 10+3 FEC, up to 3 out of 13 packets can be lost and still be recovered.

**Trade-offs:**
- Adds bandwidth overhead: `(ParityShard/DataShard) * 100%`
- Reduces latency on lossy networks by avoiding retransmissions
- More CPU usage for encoding/decoding

## Comparison with Other Protocols

| Protocol    | Transport | Congestion Control | Encryption | TLS  | Complexity |
|-------------|-----------|-------------------|------------|------|------------|
| **KCP**     | UDP       | Optional          | AES        | No   | Low        |
| TUIC        | QUIC      | BBR/Cubic         | TLS 1.3    | Yes  | Medium     |
| Hysteria    | QUIC      | Brutal            | TLS 1.3    | Yes  | Medium     |
| Hysteria2   | QUIC      | Brutal            | TLS 1.3    | Yes  | Medium     |

### When to Use KCP

**Advantages:**
- Lower overhead than QUIC (no TLS handshake)
- More tuning options for specific scenarios
- Better for environments blocking QUIC
- Simpler implementation

**Disadvantages:**
- No standardized congestion control like BBR
- Requires manual tuning for optimal performance
- No built-in connection migration
- Less efficient multiplexing than QUIC

## Implementation Notes

### Connection Multiplexing

Unlike QUIC, KCP-GO v5 doesn't have built-in stream multiplexing. This implementation uses application-level framing:
- Each KCP session is one persistent UDP connection
- TCP connections are tracked with 32-bit connection IDs
- Data is framed with: version + command + connID + length + payload
- Multiple TCP connections share the same KCP session

This provides efficient multiplexing while keeping the protocol simple.

### UDP Associations

UDP packets are multiplexed over the main KCP connection using association IDs:
- Each `ListenPacket()` call allocates a unique 16-bit association ID
- Packets are framed with the association ID and forwarded
- Association IDs are tracked in maps on both client and server

### Authentication

Authentication happens once per KCP session:
1. Client establishes KCP connection (with optional encryption)
2. Client sends auth request with UUID and SHA256 token
3. Server validates and responds with success/failure
4. On success, client can open streams and send packets
5. On failure, connection is closed

This mirrors TUIC's authentication model.

## Troubleshooting

### High Latency

1. Decrease `Interval` (e.g., 10ms)
2. Set `NoDelay` to 1
3. Increase `Resend` (e.g., 2)
4. Set `NoCongestion` to 1

### Packet Loss

1. Enable FEC (`DataShard: 10, ParityShard: 3`)
2. Increase window sizes
3. Decrease `Resend` to avoid over-retransmission
4. Keep congestion control enabled

### Connection Drops

1. Check firewall/NAT timeout settings
2. Decrease heartbeat interval
3. Ensure client and server MTU matches
4. Verify encryption keys match

### High CPU Usage

1. Increase `Interval` (e.g., 40ms)
2. Disable FEC if network is stable
3. Reduce window sizes
4. Enable `NoCongestion: 0`

## License

This implementation is part of the sing-quic project and follows the same license.