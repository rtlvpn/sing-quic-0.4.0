# KCP Implementation Notes

## Summary of Changes

This document describes the complete rewrite of the KCP protocol implementation for sing-box integration. The previous implementation had fundamental architectural flaws that made it incompatible with sing-box patterns. This version provides a clean, working implementation that properly integrates with the sing-box ecosystem.

## Problems in Original Implementation

### 1. **Layer Confusion**

The original code mixed multiple architectural layers incorrectly:

```
❌ OLD (BROKEN):
- Mixed KCP-GO's native Listener with sing-box Dialer patterns
- Tried to manually implement multiplexing over raw KCP sessions
- Authentication happened at wrong layer (mixed with encryption)
- Created two different listener patterns (KCP-GO + custom)
```

**Why this was broken:** 
- KCP-GO's `Listener.Accept()` returns `*UDPSession` which is already a stream-oriented connection
- The code tried to add another layer of "streams" on top, creating confusion
- sing-box expects clean `N.Dialer` and `N.TCPConnectionHandlerEx` interfaces, not wrapped KCP listeners

### 2. **Authentication/Encryption Confusion**

```go
// ❌ OLD: Mixed layers
type KCPEncryption struct {
    Password string  // Used for KCP transport layer
    Salt     string  // Mixed with application auth
}

// Application-level UUID auth was confused with transport encryption
```

**Why this was broken:**
- KCP's encryption is at transport layer (encrypts UDP packets)
- Application auth (UUID + password) should happen AFTER KCP session established
- The code tried to do both in one step, causing authentication failures

### 3. **Manual Multiplexing Attempt**

```go
// ❌ OLD: Tried to manually multiplex
type clientStream struct {
    streamID uint32
    input    chan []byte  // Manual data delivery
}

// Manually read/write with stream IDs in application protocol
```

**Why this was broken:**
- Added unnecessary complexity
- KCP-GO already provides reliable stream over UDP
- One KCP session can handle multiple logical connections via framing
- The manual implementation had race conditions and buffer issues

### 4. **Not sing-box Compatible**

```go
// ❌ OLD: Wrong handler interface
type ServiceHandler interface {
    N.TCPConnectionHandlerEx
    N.UDPConnectionHandlerEx
}

// But called wrong:
handler.NewConnectionEx(ctx, conn, metadata, nil)  // Wrong signature!
```

**Why this was broken:**
- sing-box handlers expect: `NewConnectionEx(ctx, conn, source, destination, closeHandler)`
- Old code passed `Metadata` struct instead of separate `Socksaddr` values
- User context wasn't properly set with `auth.ContextWithUser()`

## New Architecture

### Layer Separation

```
✅ NEW (CORRECT):

┌─────────────────────────────────────────────────┐
│           Application Layer                      │
│  (net.Conn, net.PacketConn interfaces)          │
└─────────────────────────────────────────────────┘
                      ↕
┌─────────────────────────────────────────────────┐
│         Connection Multiplexing Layer            │
│  • TCP connections: tracked by 32-bit connID    │
│  • UDP associations: tracked by 16-bit assocID  │
│  • Protocol framing with version/command/id/data│
└─────────────────────────────────────────────────┘
                      ↕
┌─────────────────────────────────────────────────┐
│          Authentication Layer                    │
│  • UUID-based identification                     │
│  • SHA256(UUID + Password) token verification   │
│  • Happens once per KCP session                 │
└─────────────────────────────────────────────────┘
                      ↕
┌─────────────────────────────────────────────────┐
│            KCP Session Layer                     │
│  • Reliable ordered delivery over UDP           │
│  • Congestion control (optional)                │
│  • Tunable parameters (MTU, windows, etc)       │
└─────────────────────────────────────────────────┘
                      ↕
┌─────────────────────────────────────────────────┐
│          Encryption Layer (Optional)             │
│  • AES encryption of UDP packets                 │
│  • Key derived from crypt string                 │
│  • FEC (Forward Error Correction) optional      │
└─────────────────────────────────────────────────┘
                      ↕
┌─────────────────────────────────────────────────┐
│              UDP Transport                       │
└─────────────────────────────────────────────────┘
```

### Client Architecture

```go
Client
  ├─ offer() → clientConnection (one per server)
  │   ├─ kcpConn: *kcp.UDPSession (one persistent session)
  │   ├─ tcpConnMap: map[connID]*clientTCPConn (multiple TCP connections)
  │   └─ udpConnMap: map[assocID]*clientUDPConn (multiple UDP associations)
  │
  ├─ authenticate() → sends UUID + SHA256(UUID+Password)
  │
  ├─ handleMessages() → background goroutine
  │   ├─ CommandConnect → delivers data to clientTCPConn
  │   ├─ CommandPacket → delivers data to clientUDPConn
  │   └─ CommandDissociate → closes connections
  │
  └─ handleHeartbeat() → keeps connection alive
```

**Key Points:**
- One `*kcp.UDPSession` per server (persistent)
- Multiple TCP connections tracked by 32-bit IDs
- Multiple UDP associations tracked by 16-bit IDs
- All communication happens over the single KCP session
- Clean separation: transport vs application protocols

### Server Architecture

```go
Service[U]
  ├─ Start(packetConn) → accepts KCP sessions
  │
  └─ For each KCP session:
      serverSession[U]
        ├─ authenticate() → verifies UUID + token
        │
        ├─ handleMessages() → background goroutine
        │   ├─ CommandConnect → creates serverTCPConn
        │   ├─ CommandPacket → delivers to serverUDPConn
        │   └─ CommandDissociate → closes connections
        │
        └─ For each connection:
            ├─ serverTCPConn[U] → implements net.Conn
            └─ serverUDPConn[U] → implements N.PacketConn
```

**Key Points:**
- Generic over user type `U` (like Hysteria2)
- Each client gets one `serverSession`
- Connections are created on-demand
- Proper sing-box handler integration with `auth.ContextWithUser()`

## Protocol Design

### Wire Format

All integers are big-endian.

#### Authentication (Client → Server)

```
+--------+--------+-----------------+--------------------------------+
| Ver(1) | Cmd(1) | UUID(16)        | Token(32)                      |
+--------+--------+-----------------+--------------------------------+
  0x01     0x00     user identifier   SHA256(UUID + Password)
```

#### Authentication Response (Server → Client)

```
+--------+
| Status |
+--------+
  0=success
  1=failure
```

#### TCP Connect (initial, with address)

```
+--------+--------+-----------+-----------+------------------+
| Ver(1) | Cmd(1) | ConnID(4) | AddrLen(2)| Address(variable)|
+--------+--------+-----------+-----------+------------------+
  0x01     0x01     connection  length      "example.com:443"
                    identifier
```

#### TCP Data (subsequent, on existing connection)

```
+--------+--------+-----------+-----------+------------------+
| Ver(1) | Cmd(1) | ConnID(4) | DataLen(2)| Data(variable)   |
+--------+--------+-----------+-----------+------------------+
  0x01     0x01     connection  length      payload
                    identifier
```

#### UDP Packet

```
+--------+--------+----------+-----------+------------------+
| Ver(1) | Cmd(1) | AssocID(2)| DataLen(2)| Data(variable)  |
+--------+--------+----------+-----------+------------------+
  0x01     0x02     assoc ID   length      UDP payload
```

#### Dissociate (close connection/association)

```
+--------+--------+-----------+
| Ver(1) | Cmd(1) | ID(2 or 4)|
+--------+--------+-----------+
  0x01     0x03     conn/assoc ID
```

#### Heartbeat

```
+--------+--------+
| Ver(1) | Cmd(1) |
+--------+--------+
  0x01     0x04
```

## Key Implementation Details

### 1. Connection Tracking

**TCP Connections:**
- Use 32-bit connection IDs
- First packet includes destination address
- Subsequent packets include only data
- Map: `map[uint32]*clientTCPConn` or `map[uint32]*serverTCPConn[U]`

**UDP Associations:**
- Use 16-bit association IDs
- Each packet includes full data
- Map: `map[uint16]*clientUDPConn` or `map[uint16]*serverUDPConn[U]`

### 2. Authentication Flow

```go
// Client side
func (c *Client) authenticate(kcpConn *kcp.UDPSession) error {
    // Generate token
    h := sha256.New()
    h.Write(c.uuid[:])
    h.Write([]byte(c.password))
    token := h.Sum(nil)
    
    // Send: version(1) + command(1) + uuid(16) + token(32)
    // Receive: status(1) where 0=success
}

// Server side
func (s *serverSession[U]) authenticate() error {
    // Read uuid and received token
    // Calculate expected token same way
    // Compare with subtle.ConstantTimeCompare()
    // Send: 0 for success, 1 for failure
}
```

**Security:**
- Uses `crypto/subtle.ConstantTimeCompare` to prevent timing attacks
- Token is SHA256(UUID + Password), 32 bytes
- One-time authentication per KCP session

### 3. Multiplexing Strategy

**Why not use KCP streams?**
KCP-GO v5 doesn't have built-in stream multiplexing like QUIC. Each `UDPSession` is one bidirectional stream.

**Solution:**
- One KCP session per client-server pair
- Application-level framing with connection/association IDs
- Data delivery via channels: `chan *buf.Buffer`
- Proper cleanup with `sync.Once` for close operations

### 4. Handler Integration

```go
// Correct sing-box integration (like Hysteria2):
ctx := auth.ContextWithUser(s.ctx, s.user)
s.service.handler.NewConnectionEx(
    ctx,
    conn,
    M.SocksaddrFromNet(s.kcpConn.RemoteAddr()).Unwrap(),  // source
    destination,                                            // destination
    nil,                                                    // closeHandler
)
```

**Key points:**
- User set in context with `auth.ContextWithUser()`
- Source is remote address of KCP connection
- Destination is parsed from protocol
- Handler runs in background goroutine

## Differences from Other Protocols

### vs TUIC

**Similar:**
- UUID-based authentication
- SHA256 token verification
- Multiplexing over one connection

**Different:**
- TUIC uses QUIC (with TLS handshake)
- KCP uses simpler UDP + custom reliability
- TUIC has built-in QUIC streams, KCP does app-level framing

### vs Hysteria2

**Similar:**
- Generic user type `Service[U comparable]`
- sing-box handler pattern
- Authentication flow

**Different:**
- Hysteria2 uses QUIC with Brutal congestion control
- Hysteria2 has Salamander obfuscation
- KCP has more tuning options (NoDelay, Interval, etc)

## Configuration Examples

### Minimal

```go
client, _ := NewClient(ClientOptions{
    Context:       context.Background(),
    Dialer:        dialer,
    ServerAddress: M.ParseSocksaddr("server:8388"),
    UUID:          uuid,
    Password:      "password",
})
```

### Production

```go
client, _ := NewClient(ClientOptions{
    Context:       context.Background(),
    Dialer:        dialer,
    ServerAddress: M.ParseSocksaddr("server:8388"),
    UUID:          uuid,
    Password:      "password",
    KCPOptions:    kcp.DefaultKCPOptions(),
    Crypt:         "strong-encryption-key",
    DataShard:     10,  // FEC
    ParityShard:   3,   // FEC
    Heartbeat:     10 * time.Second,
})
```

### Low Latency

```go
opts := kcp.AggressiveKCPOptions()  // or customize:
opts := kcp.KCPOptions{
    MTU:          1400,
    SndWnd:       512,
    RcvWnd:       2048,
    NoDelay:      1,      // No delay mode
    Interval:     10,     // 10ms update interval
    Resend:       2,      // Fast resend
    NoCongestion: 1,      // Disable congestion control
}
```

## Testing

Run tests:
```bash
cd sing-quic-0.4.0/kcp
go test -v
```

Build:
```bash
go build .
```

## Integration Checklist

When integrating with sing-box:

- [ ] Add KCP outbound type
- [ ] Add KCP inbound type
- [ ] Parse UUID from config
- [ ] Parse KCP options from JSON
- [ ] Handle user management (UpdateUsers)
- [ ] Create proper N.Dialer implementation
- [ ] Create proper ServerHandler implementation
- [ ] Add to protocol list in sing-box

## Performance Tuning Guide

### Good Network (Low latency, minimal loss)
```go
KCPOptions{
    MTU: 1400, SndWnd: 512, RcvWnd: 2048,
    NoDelay: 1, Interval: 10, Resend: 2, NoCongestion: 1,
}
```

### Lossy Network (Packet loss > 5%)
```go
KCPOptions{
    MTU: 1350, SndWnd: 256, RcvWnd: 1024,
    NoDelay: 0, Interval: 40, Resend: 1, NoCongestion: 0,
}
// Enable FEC: DataShard: 10, ParityShard: 3
```

### Restricted Network (Low bandwidth)
```go
KCPOptions{
    MTU: 1200, SndWnd: 64, RcvWnd: 256,
    NoDelay: 0, Interval: 50, Resend: 1, NoCongestion: 0,
}
```

## Known Limitations

1. **No connection migration:** Unlike QUIC, KCP sessions are tied to UDP 5-tuple
2. **No 0-RTT:** Always requires authentication handshake
3. **Manual tuning:** Optimal parameters depend on network conditions
4. **CPU usage:** More aggressive settings = higher CPU usage

## Future Improvements

Potential enhancements (not implemented):

- [ ] Adaptive parameter tuning based on RTT/loss
- [ ] Connection pooling for very high connection rate
- [ ] QUIC-style connection migration
- [ ] Optional obfuscation layer
- [ ] Bandwidth limiting like Hysteria2
- [ ] Better error reporting with codes

## Troubleshooting

### "authentication failed"
- Check UUID matches between client and server
- Check password matches
- Ensure server has user registered (`UpdateUsers()`)

### High CPU usage
- Decrease Interval (20ms → 40ms)
- Disable FEC if not needed
- Enable congestion control (NoCongestion: 0)

### Connection drops
- Increase heartbeat frequency
- Check firewall/NAT timeouts
- Verify MTU is not too large for network path

### High latency
- Set NoDelay: 1
- Decrease Interval (20ms → 10ms)
- Increase Resend (2 → 3)
- Disable congestion control (NoCongestion: 1)

## License

Same as sing-quic project.