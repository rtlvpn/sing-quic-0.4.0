# Migration Guide: KCP Implementation v1 → v2

This guide helps you migrate from the old broken KCP implementation to the new clean architecture.

## Why Migrate?

The old implementation had fundamental issues:
- ❌ Mixed KCP-GO listener with sing-box patterns incorrectly
- ❌ Confused authentication and encryption layers
- ❌ Incompatible with sing-box handler interfaces
- ❌ Race conditions and connection leaks
- ❌ Hallucinated multiplexing layer

The new implementation:
- ✅ Clean layer separation (UDP → KCP → Auth → Multiplex → App)
- ✅ Proper sing-box integration
- ✅ Compatible with other protocols (Hysteria2, TUIC patterns)
- ✅ Stable and tested

## Breaking Changes Summary

| Category | Old | New |
|----------|-----|-----|
| Config Type | `KCPConfig` | `KCPOptions` |
| Encryption | `KCPEncryption{Password, Salt}` | `Crypt` (single key) |
| Service Type | `Service` | `Service[U comparable]` |
| Handler Interface | Wrong signature | Fixed signature |
| Client Method | `getSession()` | `offer()` (internal) |
| Connection Type | `ConnectionType` enum | Removed (always multiplexed) |

## Configuration Changes

### Old Configuration (Broken)

```go
type ClientOptions struct {
    Context         context.Context
    Dialer          N.Dialer
    ServerAddress   M.Socksaddr
    UUID            [16]byte
    Password        string
    KCPConfig       KCPConfig      // ❌ Old name
    Encryption      KCPEncryption  // ❌ Complex structure
    Heartbeat       time.Duration
    UDPTimeout      time.Duration  // ❌ Not needed
}

type KCPConfig struct {
    MTU             int
    SndWnd          int
    RcvWnd          int
    NoDelay         int
    Interval        int
    Resend          int
    NoCongestion    int
    ACKNoDelay      bool    // ❌ Removed
    StreamMode      bool    // ❌ Removed
    AutoExpire      int     // ❌ Removed
    WriteDelay      bool    // ❌ Removed
    ConnectionType  ConnectionType  // ❌ Removed
}

type KCPEncryption struct {
    Password    string  // ❌ Confusing
    Salt        string  // ❌ Confusing
    DataShard   int
    ParityShard int
}
```

### New Configuration (Fixed)

```go
type ClientOptions struct {
    Context       context.Context
    Dialer        N.Dialer
    ServerAddress M.Socksaddr
    UUID          [16]byte
    Password      string
    KCPOptions    KCPOptions  // ✅ Renamed
    Crypt         string      // ✅ Simple encryption key
    DataShard     int         // ✅ Direct FEC options
    ParityShard   int
    Heartbeat     time.Duration
}

type KCPOptions struct {
    MTU          int  // Simplified to core options
    SndWnd       int
    RcvWnd       int
    NoDelay      int  // 0 or 1
    Interval     int  // milliseconds
    Resend       int
    NoCongestion int  // 0 or 1
}
```

## Migration Steps

### Step 1: Update Client Code

**Before:**
```go
client, err := kcp.NewClient(kcp.ClientOptions{
    Context:       ctx,
    Dialer:        dialer,
    ServerAddress: serverAddr,
    UUID:          uuid,
    Password:      "mypassword",
    KCPConfig: kcp.KCPConfig{
        MTU:            1400,
        SndWnd:         128,
        RcvWnd:         512,
        NoDelay:        0x10,
        Interval:       20,
        Resend:         2,
        NoCongestion:   0,
        ACKNoDelay:     true,     // ❌ Remove
        StreamMode:     false,    // ❌ Remove
        AutoExpire:     900,      // ❌ Remove
        WriteDelay:     false,    // ❌ Remove
        ConnectionType: kcp.MultiplexedConnection,  // ❌ Remove
    },
    Encryption: kcp.KCPEncryption{
        Password:    "kcppass",  // ❌ Change
        Salt:        "kcpsalt",  // ❌ Change
        DataShard:   10,
        ParityShard: 3,
    },
    Heartbeat:  10 * time.Second,
    UDPTimeout: 30 * time.Second,  // ❌ Remove
})
```

**After:**
```go
client, err := kcp.NewClient(kcp.ClientOptions{
    Context:       ctx,
    Dialer:        dialer,
    ServerAddress: serverAddr,
    UUID:          uuid,
    Password:      "mypassword",
    KCPOptions:    kcp.DefaultKCPOptions(),  // ✅ Use preset or customize
    Crypt:         "my-encryption-key",      // ✅ Single key
    DataShard:     10,                       // ✅ Direct options
    ParityShard:   3,
    Heartbeat:     10 * time.Second,
})
```

### Step 2: Update Server Code

**Before:**
```go
service, err := kcp.NewService(kcp.ServiceOptions{
    Context:     ctx,
    Logger:      logger,
    KCPConfig:   kcpConfig,
    Encryption:  encryption,
    AuthTimeout: 3 * time.Second,  // ❌ Remove
    Heartbeat:   10 * time.Second,
    UDPTimeout:  30 * time.Second, // ❌ Remove
    Handler:     handler,
})

service.UpdateUsers(
    []any{user1, user2},        // ❌ Wrong type
    [][16]byte{uuid1, uuid2},
    []string{"pass1", "pass2"},
)

err = service.Start("0.0.0.0:8388")  // ❌ Wrong signature
```

**After:**
```go
service, err := kcp.NewService[string](kcp.ServiceOptions{  // ✅ Generic type
    Context:     ctx,
    Logger:      logger,
    KCPOptions:  kcp.DefaultKCPOptions(),
    Crypt:       "my-encryption-key",
    DataShard:   10,
    ParityShard: 3,
    Heartbeat:   10 * time.Second,
    Handler:     handler,
})

service.UpdateUsers(
    []string{"user1", "user2"},  // ✅ Typed
    [][16]byte{uuid1, uuid2},
    []string{"pass1", "pass2"},
)

// ✅ Create UDP listener first
udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:8388")
udpConn, _ := net.ListenUDP("udp", udpAddr)
err = service.Start(udpConn)
```

### Step 3: Update Handler Interface

**Before:**
```go
type MyHandler struct{}

func (h *MyHandler) NewConnectionEx(
    ctx context.Context,
    conn net.Conn,
    metadata M.Metadata,  // ❌ Wrong
    closeHandler N.CloseHandlerFunc,
) error {
    // Handle connection
    return nil
}
```

**After:**
```go
type MyHandler struct{}

func (h *MyHandler) NewConnectionEx(
    ctx context.Context,
    conn net.Conn,
    source M.Socksaddr,      // ✅ Correct
    destination M.Socksaddr,  // ✅ Correct
    closeHandler N.CloseHandlerFunc,
) {
    // Handle connection
    // Note: no return value
}
```

### Step 4: Update Encryption Key

**Before:**
```go
// Old: Password + Salt combined
encryption := kcp.KCPEncryption{
    Password: "mypassword",
    Salt:     "mysalt",
}
// Internally: block, _ := kcp.NewAESBlockCrypt([]byte(password + salt))
```

**After:**
```go
// New: Single key, SHA256 hashed internally
crypt := "my-encryption-key"
// Internally: key := sha256.Sum256([]byte(crypt))
//             block, _ := kcp.NewAESBlockCrypt(key[:16])
```

**Migration tip:** If you want the same key as before:
```go
oldKey := "mypassword" + "mysalt"
newCrypt := oldKey  // Use the combined string
```

### Step 5: Update JSON Configuration

**Before:**
```json
{
  "kcp_config": {
    "mtu": 1400,
    "snd_wnd": 128,
    "rcv_wnd": 512,
    "nodelay": 16,
    "interval": 20,
    "resend": 2,
    "no_congestion": 0,
    "ack_no_delay": true,
    "stream_mode": false,
    "auto_expire": 900,
    "write_delay": false
  },
  "encryption": {
    "password": "kcppass",
    "salt": "kcpsalt",
    "data_shard": 10,
    "parity_shard": 3
  }
}
```

**After:**
```json
{
  "kcp": {
    "mtu": 1400,
    "snd_wnd": 128,
    "rcv_wnd": 512,
    "nodelay": 1,
    "interval": 20,
    "resend": 2,
    "nc": 0
  },
  "crypt": "my-encryption-key",
  "fec": {
    "data_shard": 10,
    "parity_shard": 3
  }
}
```

## Common Migration Issues

### Issue 1: "undefined: KCPConfig"

**Error:**
```
undefined: KCPConfig
```

**Solution:**
```go
// Change
config := kcp.KCPConfig{...}

// To
options := kcp.KCPOptions{...}
// Or use preset
options := kcp.DefaultKCPOptions()
```

### Issue 2: "undefined: KCPEncryption"

**Error:**
```
undefined: KCPEncryption
```

**Solution:**
```go
// Change
encryption := kcp.KCPEncryption{
    Password: "pass",
    Salt: "salt",
}

// To
crypt := "pass"  // Or "pass" + "salt" if you want same key
```

### Issue 3: "cannot use service (type *Service) as type *Service[string]"

**Error:**
```
cannot use service (type *Service) as type *Service[string]
```

**Solution:**
```go
// Change
service, err := kcp.NewService(options)

// To
service, err := kcp.NewService[string](options)
// Or use your user type
service, err := kcp.NewService[*User](options)
```

### Issue 4: "service.Start undefined (type *Service has no method Start)"

**Error:**
```
service.Start undefined
```

**Solution:**
```go
// Old API (broken):
err = service.Start("0.0.0.0:8388")

// New API (correct):
udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:8388")
udpConn, _ := net.ListenUDP("udp", udpAddr)
err = service.Start(udpConn)
```

### Issue 5: "ConnectionType not defined"

**Error:**
```
undefined: SingleConnection or MultiplexedConnection
```

**Solution:**
```go
// Remove ConnectionType entirely - always multiplexed now
// Just delete lines like:
// ConnectionType: kcp.MultiplexedConnection,
```

### Issue 6: Authentication Failures After Migration

**Problem:** Old code used `password + salt` as encryption key, causing auth issues.

**Solution:**
```go
// Option A: Update encryption key to match
oldPassword := "kcppass"
oldSalt := "kcpsalt"
newCrypt := oldPassword + oldSalt  // "kcppasskcpsalt"

// Option B: Use new key (must match on both client and server)
newCrypt := "my-new-encryption-key"
```

## Preset Configurations

Instead of manually configuring everything, use presets:

```go
// Balanced (default)
opts := kcp.DefaultKCPOptions()

// For unstable networks
opts := kcp.ConservativeKCPOptions()

// For good networks (low latency)
opts := kcp.AggressiveKCPOptions()

// Custom
opts := kcp.KCPOptions{
    MTU:          1350,
    SndWnd:       256,
    RcvWnd:       1024,
    NoDelay:      1,
    Interval:     20,
    Resend:       2,
    NoCongestion: 1,
}
```

## Testing Your Migration

### 1. Build Test
```bash
cd sing-quic-0.4.0/kcp
go build .
```

### 2. Run Tests
```bash
go test -v
```

### 3. Integration Test
```go
// Create test client and server
func TestMigration(t *testing.T) {
    // Setup server
    service, _ := kcp.NewService[string](kcp.ServiceOptions{
        Context:    context.Background(),
        Logger:     logger.NOP(),
        KCPOptions: kcp.DefaultKCPOptions(),
        Crypt:      "test-key",
        Handler:    &testHandler{},
    })
    
    udpAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
    udpConn, _ := net.ListenUDP("udp", udpAddr)
    service.Start(udpConn)
    
    // Setup client
    client, _ := kcp.NewClient(kcp.ClientOptions{
        Context:       context.Background(),
        Dialer:        dialer,
        ServerAddress: M.SocksaddrFromNet(udpConn.LocalAddr()),
        UUID:          testUUID,
        Password:      "test-pass",
        KCPOptions:    kcp.DefaultKCPOptions(),
        Crypt:         "test-key",
    })
    
    // Test connection
    conn, err := client.DialConn(context.Background(), 
        M.ParseSocksaddr("google.com:80"))
    if err != nil {
        t.Fatal(err)
    }
    defer conn.Close()
}
```

## Rollback Plan

If you need to temporarily rollback:

1. **Keep old version:**
   ```bash
   # Before migration
   cp -r kcp kcp.backup
   ```

2. **Use git:**
   ```bash
   # Find commit before rewrite
   git log --oneline kcp/
   git checkout <commit> -- kcp/
   ```

3. **Version tag:**
   ```bash
   # Tag current working version before migration
   git tag -a kcp-v1-working -m "Last working v1"
   ```

## Support

If you encounter issues during migration:

1. Check `IMPLEMENTATION_NOTES.md` for architecture details
2. Review `example_test.go` for working examples
3. Read `README.md` for full documentation
4. Check diagnostics: `go build . && go test -v`

## Checklist

Migration complete when:

- [ ] Updated to `KCPOptions` from `KCPConfig`
- [ ] Changed `KCPEncryption` to `Crypt` string
- [ ] Added generic type parameter `[U]` to `NewService`
- [ ] Updated handler interface signatures
- [ ] Changed `service.Start(addr)` to `service.Start(conn)`
- [ ] Removed deprecated options (ACKNoDelay, StreamMode, etc)
- [ ] Updated JSON configuration
- [ ] Tests pass: `go test -v`
- [ ] Code compiles: `go build .`
- [ ] Integration test successful

## Version Compatibility

| Component | Old (v1) | New (v2) | Compatible? |
|-----------|----------|----------|-------------|
| Wire Protocol | Custom | Custom | ❌ No |
| Authentication | UUID+Token | UUID+Token | ✅ Yes* |
| Encryption Key | Pass+Salt | SHA256(Crypt) | ⚠️ Must migrate |
| API | Broken | Fixed | ❌ No |
| sing-box | Incompatible | Compatible | N/A |

*Authentication compatible if encryption keys match

## Final Notes

- **No backward compatibility:** Wire protocol changed, old clients can't connect to new servers
- **Upgrade atomically:** Update both client and server together
- **Test thoroughly:** Run integration tests after migration
- **Monitor logs:** Check for authentication failures after deployment

Good luck with your migration! 🚀