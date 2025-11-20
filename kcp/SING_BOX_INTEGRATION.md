# KCP Integration Guide for sing-box

This is a comprehensive guide for integrating the KCP protocol implementation into sing-box. Follow these steps to add KCP support to sing-box.

---

## Table of Contents

1. [Overview](#overview)
2. [Prerequisites](#prerequisites)
3. [File Structure](#file-structure)
4. [Step 1: Add Dependencies](#step-1-add-dependencies)
5. [Step 2: Create Protocol Options](#step-2-create-protocol-options)
6. [Step 3: Implement Outbound (Client)](#step-3-implement-outbound-client)
7. [Step 4: Implement Inbound (Server)](#step-4-implement-inbound-server)
8. [Step 5: Register Protocol](#step-5-register-protocol)
9. [Step 6: Configuration Examples](#step-6-configuration-examples)
10. [Step 7: Testing](#step-7-testing)
11. [Troubleshooting](#troubleshooting)
12. [Complete Code Reference](#complete-code-reference)

---

## Overview

The KCP implementation follows the same patterns as other protocols in sing-box (Hysteria2, TUIC):
- **Outbound**: Client side that dials to KCP servers
- **Inbound**: Server side that accepts KCP connections
- **Options**: JSON-serializable configuration structures
- **Handler**: Routes connections through sing-box's routing system

### What You'll Build

```
sing-box/
├── option/
│   └── kcp.go          (Configuration structures)
├── outbound/
│   └── kcp.go          (Client implementation)
├── inbound/
│   └── kcp.go          (Server implementation)
└── protocol/kcp/
    └── kcp.go          (Protocol registration)
```

---

## Prerequisites

1. **sing-box source code** - Clone from github.com/SagerNet/sing-box
2. **This KCP library** - Already in sing-quic-0.4.0/kcp
3. **Go 1.20+** - For building

---

## File Structure

### sing-box Directory Layout

```
sing-box/
├── option/
│   ├── inbound.go      (Add KCPInboundOptions)
│   ├── outbound.go     (Add KCPOutboundOptions)
│   └── kcp.go          (NEW - KCP-specific options)
├── inbound/
│   ├── builder.go      (Register KCP)
│   └── kcp.go          (NEW - KCP inbound)
├── outbound/
│   ├── builder.go      (Register KCP)
│   └── kcp.go          (NEW - KCP outbound)
└── protocol/kcp/
    └── kcp.go          (NEW - Protocol utilities)
```

---

## Step 1: Add Dependencies

### Update go.mod

```bash
cd sing-box
go get github.com/sagernet/sing-quic@latest
go get github.com/xtaci/kcp-go/v5@latest
```

The KCP library is already part of sing-quic, so this will pull it in.

---

## Step 2: Create Protocol Options

### File: `option/kcp.go`

```go
package option

import "time"

// KCPOptions contains KCP protocol tuning parameters
type KCPOptions struct {
    MTU          int  `json:"mtu,omitempty"`           // Maximum Transmission Unit (default: 1350)
    SndWnd       int  `json:"snd_wnd,omitempty"`       // Send window size (default: 128)
    RcvWnd       int  `json:"rcv_wnd,omitempty"`       // Receive window size (default: 512)
    NoDelay      int  `json:"nodelay,omitempty"`       // 0=normal, 1=no delay (default: 1)
    Interval     int  `json:"interval,omitempty"`      // Update interval in ms (default: 20)
    Resend       int  `json:"resend,omitempty"`        // Fast resend mode (default: 2)
    NoCongestion int  `json:"nc,omitempty"`            // Disable congestion control (default: 1)
}

// KCPFECOptions contains Forward Error Correction settings
type KCPFECOptions struct {
    DataShard   int `json:"data_shard,omitempty"`    // FEC data shards
    ParityShard int `json:"parity_shard,omitempty"`  // FEC parity shards
}

// KCPOutboundOptions defines KCP outbound configuration
type KCPOutboundOptions struct {
    DialerOptions
    ServerOptions
    UUID            string           `json:"uuid"`
    Password        string           `json:"password,omitempty"`
    KCP             *KCPOptions      `json:"kcp,omitempty"`
    Crypt           string           `json:"crypt,omitempty"`
    FEC             *KCPFECOptions   `json:"fec,omitempty"`
    Heartbeat       Duration         `json:"heartbeat,omitempty"`
}

// KCPInboundOptions defines KCP inbound configuration
type KCPInboundOptions struct {
    ListenOptions
    Users           []User           `json:"users,omitempty"`
    KCP             *KCPOptions      `json:"kcp,omitempty"`
    Crypt           string           `json:"crypt,omitempty"`
    FEC             *KCPFECOptions   `json:"fec,omitempty"`
    Heartbeat       Duration         `json:"heartbeat,omitempty"`
}

// User represents a KCP user with UUID and password
type User struct {
    Name     string `json:"name,omitempty"`
    UUID     string `json:"uuid"`
    Password string `json:"password,omitempty"`
}
```

### Update `option/outbound.go`

Add to the `Outbound` struct:

```go
type Outbound struct {
    // ... existing fields ...
    KCPOptions *KCPOutboundOptions `json:"-"`
}
```

Add to `UnmarshalJSON`:

```go
case C.TypeKCP:
    var kcpOptions KCPOutboundOptions
    err = json.Unmarshal(rawOptions, &kcpOptions)
    options.KCPOptions = &kcpOptions
```

### Update `option/inbound.go`

Add to the `Inbound` struct:

```go
type Inbound struct {
    // ... existing fields ...
    KCPOptions *KCPInboundOptions `json:"-"`
}
```

Add to `UnmarshalJSON`:

```go
case C.TypeKCP:
    var kcpOptions KCPInboundOptions
    err = json.Unmarshal(rawOptions, &kcpOptions)
    options.KCPOptions = &kcpOptions
```

---

## Step 3: Implement Outbound (Client)

### File: `outbound/kcp.go`

```go
package outbound

import (
    "context"
    "net"
    
    "github.com/sagernet/sing-box/adapter"
    "github.com/sagernet/sing-box/common/dialer"
    "github.com/sagernet/sing-box/log"
    "github.com/sagernet/sing-box/option"
    "github.com/sagernet/sing-quic/kcp"
    "github.com/sagernet/sing/common"
    "github.com/sagernet/sing/common/buf"
    E "github.com/sagernet/sing/common/exceptions"
    M "github.com/sagernet/sing/common/metadata"
    N "github.com/sagernet/sing/common/network"
    
    "github.com/gofrs/uuid/v5"
)

var _ adapter.Outbound = (*KCP)(nil)

type KCP struct {
    myOutboundAdapter
    client *kcp.Client
}

func NewKCP(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.KCPOutboundOptions) (*KCP, error) {
    // Parse UUID
    userUUID, err := uuid.FromString(options.UUID)
    if err != nil {
        return nil, E.Cause(err, "invalid uuid")
    }
    var uuidBytes [16]byte
    copy(uuidBytes[:], userUUID.Bytes())
    
    // Build KCP options
    kcpOptions := kcp.DefaultKCPOptions()
    if options.KCP != nil {
        if options.KCP.MTU > 0 {
            kcpOptions.MTU = options.KCP.MTU
        }
        if options.KCP.SndWnd > 0 {
            kcpOptions.SndWnd = options.KCP.SndWnd
        }
        if options.KCP.RcvWnd > 0 {
            kcpOptions.RcvWnd = options.KCP.RcvWnd
        }
        kcpOptions.NoDelay = options.KCP.NoDelay
        if options.KCP.Interval > 0 {
            kcpOptions.Interval = options.KCP.Interval
        }
        if options.KCP.Resend > 0 {
            kcpOptions.Resend = options.KCP.Resend
        }
        kcpOptions.NoCongestion = options.KCP.NoCongestion
    }
    
    // Build FEC options
    dataShard := 0
    parityShard := 0
    if options.FEC != nil {
        dataShard = options.FEC.DataShard
        parityShard = options.FEC.ParityShard
    }
    
    // Get heartbeat
    heartbeat := options.Heartbeat.Duration()
    if heartbeat == 0 {
        heartbeat = 10 * time.Second
    }
    
    // Create dialer
    outboundDialer, err := dialer.New(router, options.DialerOptions)
    if err != nil {
        return nil, err
    }
    
    // Create KCP client
    client, err := kcp.NewClient(kcp.ClientOptions{
        Context:       ctx,
        Dialer:        outboundDialer,
        ServerAddress: options.ServerOptions.Build(),
        UUID:          uuidBytes,
        Password:      options.Password,
        KCPOptions:    kcpOptions,
        Crypt:         options.Crypt,
        DataShard:     dataShard,
        ParityShard:   parityShard,
        Heartbeat:     heartbeat,
    })
    if err != nil {
        return nil, err
    }
    
    return &KCP{
        myOutboundAdapter: myOutboundAdapter{
            protocol: C.TypeKCP,
            network:  []string{N.NetworkTCP, N.NetworkUDP},
            router:   router,
            logger:   logger,
            tag:      tag,
        },
        client: client,
    }, nil
}

func (h *KCP) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
    switch N.NetworkName(network) {
    case N.NetworkTCP:
        h.logger.InfoContext(ctx, "outbound connection to ", destination)
        return h.client.DialConn(ctx, destination)
    case N.NetworkUDP:
        conn, err := h.client.ListenPacket(ctx, destination)
        if err != nil {
            return nil, err
        }
        return bufio.NewBindPacketConn(conn, destination), nil
    default:
        return nil, E.New("unsupported network: ", network)
    }
}

func (h *KCP) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
    h.logger.InfoContext(ctx, "outbound packet connection to ", destination)
    return h.client.ListenPacket(ctx, destination)
}

func (h *KCP) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
    return NewConnection(ctx, h, conn, metadata)
}

func (h *KCP) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
    return NewPacketConnection(ctx, h, conn, metadata)
}

func (h *KCP) Close() error {
    return common.Close(h.client)
}
```

---

## Step 4: Implement Inbound (Server)

### File: `inbound/kcp.go`

```go
package inbound

import (
    "context"
    "net"
    "time"
    
    "github.com/sagernet/sing-box/adapter"
    "github.com/sagernet/sing-box/common/uot"
    "github.com/sagernet/sing-box/log"
    "github.com/sagernet/sing-box/option"
    "github.com/sagernet/sing-quic/kcp"
    "github.com/sagernet/sing/common"
    "github.com/sagernet/sing/common/auth"
    "github.com/sagernet/sing/common/buf"
    E "github.com/sagernet/sing/common/exceptions"
    M "github.com/sagernet/sing/common/metadata"
    N "github.com/sagernet/sing/common/network"
    
    "github.com/gofrs/uuid/v5"
)

var _ adapter.Inbound = (*KCP)(nil)

type KCP struct {
    myInboundAdapter
    ctx     context.Context
    router  adapter.Router
    logger  log.ContextLogger
    service *kcp.Service[int]
    users   []option.User
}

func NewKCP(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.KCPInboundOptions) (*KCP, error) {
    // Build KCP options
    kcpOptions := kcp.DefaultKCPOptions()
    if options.KCP != nil {
        if options.KCP.MTU > 0 {
            kcpOptions.MTU = options.KCP.MTU
        }
        if options.KCP.SndWnd > 0 {
            kcpOptions.SndWnd = options.KCP.SndWnd
        }
        if options.KCP.RcvWnd > 0 {
            kcpOptions.RcvWnd = options.KCP.RcvWnd
        }
        kcpOptions.NoDelay = options.KCP.NoDelay
        if options.KCP.Interval > 0 {
            kcpOptions.Interval = options.KCP.Interval
        }
        if options.KCP.Resend > 0 {
            kcpOptions.Resend = options.KCP.Resend
        }
        kcpOptions.NoCongestion = options.KCP.NoCongestion
    }
    
    // Build FEC options
    dataShard := 0
    parityShard := 0
    if options.FEC != nil {
        dataShard = options.FEC.DataShard
        parityShard = options.FEC.ParityShard
    }
    
    // Get heartbeat
    heartbeat := options.Heartbeat.Duration()
    if heartbeat == 0 {
        heartbeat = 10 * time.Second
    }
    
    inbound := &KCP{
        myInboundAdapter: myInboundAdapter{
            protocol: C.TypeKCP,
            network:  []string{N.NetworkTCP, N.NetworkUDP},
            ctx:      ctx,
            router:   router,
            logger:   logger,
            tag:      tag,
            listenOptions: options.ListenOptions,
        },
        ctx:    ctx,
        router: router,
        logger: logger,
        users:  options.Users,
    }
    
    // Create KCP service
    service, err := kcp.NewService[int](kcp.ServiceOptions{
        Context:     ctx,
        Logger:      logger,
        KCPOptions:  kcpOptions,
        Crypt:       options.Crypt,
        DataShard:   dataShard,
        ParityShard: parityShard,
        Heartbeat:   heartbeat,
        Handler:     inbound,
    })
    if err != nil {
        return nil, err
    }
    
    inbound.service = service
    
    return inbound, nil
}

func (h *KCP) Start() error {
    // Parse users
    if len(h.users) == 0 {
        return E.New("missing users")
    }
    
    userList := make([]int, 0, len(h.users))
    uuidList := make([][16]byte, 0, len(h.users))
    passwordList := make([]string, 0, len(h.users))
    
    for i, user := range h.users {
        userUUID, err := uuid.FromString(user.UUID)
        if err != nil {
            return E.Cause(err, "invalid uuid for user ", i)
        }
        
        var uuidBytes [16]byte
        copy(uuidBytes[:], userUUID.Bytes())
        
        userList = append(userList, i)
        uuidList = append(uuidList, uuidBytes)
        passwordList = append(passwordList, user.Password)
    }
    
    // Update service users
    h.service.UpdateUsers(userList, uuidList, passwordList)
    
    // Start UDP listener
    udpConn, err := h.myInboundAdapter.ListenUDP()
    if err != nil {
        return err
    }
    
    // Start service
    return h.service.Start(udpConn)
}

func (h *KCP) Close() error {
    return common.Close(h.service)
}

// NewConnectionEx implements ServerHandler interface
func (h *KCP) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
    // Get user from context
    userIndex := auth.UserFromContext[int](ctx)
    var userName string
    if userIndex >= 0 && userIndex < len(h.users) {
        userName = h.users[userIndex].Name
        if userName == "" {
            userName = h.users[userIndex].UUID
        }
    }
    
    h.logger.InfoContext(ctx, "[", userName, "] inbound connection from ", source)
    h.logger.InfoContext(ctx, "[", userName, "] inbound connection to ", destination)
    
    metadata := adapter.InboundContext{
        Inbound:     h.tag,
        InboundType: h.protocol,
        Source:      source,
        Destination: destination,
        User:        userName,
    }
    
    h.router.RouteConnection(ctx, conn, metadata)
}

// NewPacketConnectionEx implements ServerHandler interface
func (h *KCP) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
    // Get user from context
    userIndex := auth.UserFromContext[int](ctx)
    var userName string
    if userIndex >= 0 && userIndex < len(h.users) {
        userName = h.users[userIndex].Name
        if userName == "" {
            userName = h.users[userIndex].UUID
        }
    }
    
    h.logger.InfoContext(ctx, "[", userName, "] inbound packet connection from ", source)
    
    metadata := adapter.InboundContext{
        Inbound:     h.tag,
        InboundType: h.protocol,
        Source:      source,
        User:        userName,
    }
    
    h.router.RoutePacketConnection(ctx, conn, metadata)
}
```

---

## Step 5: Register Protocol

### Update `constant/protocol.go`

Add KCP to protocol constants:

```go
const (
    // ... existing protocols ...
    TypeKCP = "kcp"
)
```

### Update `outbound/builder.go`

Add KCP case in `New()` function:

```go
case C.TypeKCP:
    return NewKCP(ctx, router, logger, options.Tag, *options.KCPOptions)
```

### Update `inbound/builder.go`

Add KCP case in `New()` function:

```go
case C.TypeKCP:
    return NewKCP(ctx, router, logger, options.Tag, *options.KCPOptions)
```

---

## Step 6: Configuration Examples

### Client Configuration

```json
{
  "outbounds": [
    {
      "type": "kcp",
      "tag": "kcp-out",
      "server": "server.example.com",
      "server_port": 8388,
      "uuid": "12345678-1234-1234-1234-123456789abc",
      "password": "my-password",
      "kcp": {
        "mtu": 1350,
        "snd_wnd": 128,
        "rcv_wnd": 512,
        "nodelay": 1,
        "interval": 20,
        "resend": 2,
        "nc": 1
      },
      "crypt": "encryption-key",
      "fec": {
        "data_shard": 10,
        "parity_shard": 3
      },
      "heartbeat": "10s"
    }
  ]
}
```

### Server Configuration

```json
{
  "inbounds": [
    {
      "type": "kcp",
      "tag": "kcp-in",
      "listen": "::",
      "listen_port": 8388,
      "users": [
        {
          "name": "user1",
          "uuid": "12345678-1234-1234-1234-123456789abc",
          "password": "my-password"
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
      "crypt": "encryption-key",
      "fec": {
        "data_shard": 10,
        "parity_shard": 3
      },
      "heartbeat": "10s"
    }
  ]
}
```

---

## Step 7: Testing

### Build sing-box

```bash
cd sing-box
go build -tags with_kcp -o sing-box ./cmd/sing-box
```

### Test Client

Create `client-config.json`:

```json
{
  "log": {
    "level": "debug"
  },
  "inbounds": [
    {
      "type": "mixed",
      "listen": "127.0.0.1",
      "listen_port": 1080
    }
  ],
  "outbounds": [
    {
      "type": "kcp",
      "tag": "kcp-out",
      "server": "127.0.0.1",
      "server_port": 8388,
      "uuid": "12345678-1234-1234-1234-123456789abc",
      "password": "test-password",
      "crypt": "test-key"
    }
  ]
}
```

### Test Server

Create `server-config.json`:

```json
{
  "log": {
    "level": "debug"
  },
  "inbounds": [
    {
      "type": "kcp",
      "listen": "127.0.0.1",
      "listen_port": 8388,
      "users": [
        {
          "uuid": "12345678-1234-1234-1234-123456789abc",
          "password": "test-password"
        }
      ],
      "crypt": "test-key"
    }
  ],
  "outbounds": [
    {
      "type": "direct"
    }
  ]
}
```

### Run Tests

```bash
# Terminal 1: Start server
./sing-box run -c server-config.json

# Terminal 2: Start client
./sing-box run -c client-config.json

# Terminal 3: Test connection
curl -x socks5://127.0.0.1:1080 http://example.com
```

---

## Troubleshooting

### Common Issues

#### 1. "undefined: C.TypeKCP"

**Solution:** Add `TypeKCP = "kcp"` to `constant/protocol.go`

#### 2. "cannot use options.KCPOptions (variable of pointer to option.KCPOutboundOptions)"

**Solution:** Check that `UnmarshalJSON` is correctly setting the options field

#### 3. "authentication failed"

**Solution:** 
- Verify UUID matches between client and server
- Check password matches
- Ensure crypt key is identical on both sides

#### 4. "bind: address already in use"

**Solution:** Change the port or stop other services using that port

#### 5. Build errors with tags

**Solution:** Make sure to build with appropriate tags if you've gated KCP behind build tags

### Debug Mode

Enable debug logging to see detailed information:

```json
{
  "log": {
    "level": "trace",
    "timestamp": true
  }
}
```

---

## Complete Code Reference

### Minimal Working Example

If you want the absolute minimum to get started, here's what you need:

**1. Add to `constant/protocol.go`:**
```go
TypeKCP = "kcp"
```

**2. Create `option/kcp.go`** (see Step 2)

**3. Create `outbound/kcp.go`** (see Step 3)

**4. Create `inbound/kcp.go`** (see Step 4)

**5. Update builders** (see Step 5)

**6. Build and test** (see Step 7)

---

## Performance Optimization

### For Low Latency

```json
{
  "kcp": {
    "mtu": 1400,
    "snd_wnd": 512,
    "rcv_wnd": 2048,
    "nodelay": 1,
    "interval": 10,
    "resend": 2,
    "nc": 1
  }
}
```

### For Unstable Networks

```json
{
  "kcp": {
    "mtu": 1350,
    "snd_wnd": 256,
    "rcv_wnd": 1024,
    "nodelay": 0,
    "interval": 40,
    "resend": 1,
    "nc": 0
  },
  "fec": {
    "data_shard": 10,
    "parity_shard": 3
  }
}
```

---

## Additional Features (Optional)

### Add Presets Support

You can add preset configurations for common scenarios:

```go
func (o *KCPOptions) ApplyPreset(preset string) {
    switch preset {
    case "fast":
        o.MTU = 1400
        o.SndWnd = 512
        o.RcvWnd = 2048
        o.NoDelay = 1
        o.Interval = 10
        o.Resend = 2
        o.NoCongestion = 1
    case "stable":
        o.MTU = 1350
        o.SndWnd = 256
        o.RcvWnd = 1024
        o.NoDelay = 0
        o.Interval = 40
        o.Resend = 1
        o.NoCongestion = 0
    }
}
```

Then in config:

```json
{
  "kcp": {
    "preset": "fast"
  }
}
```

---

## Final Checklist

- [ ] Added `TypeKCP` to constants
- [ ] Created `option/kcp.go` with configuration structures
- [ ] Created `outbound/kcp.go` with client implementation
- [ ] Created `inbound/kcp.go` with server implementation
- [ ] Updated `outbound/builder.go` to register KCP
- [ ] Updated `inbound/builder.go` to register KCP
- [ ] Updated `option/outbound.go` with KCP case
- [ ] Updated `option/inbound.go` with KCP case
- [ ] Tested client → server connection
- [ ] Tested with real traffic (curl, etc)
- [ ] Verified logs show correct user information
- [ ] Performance tested with different KCP settings

---

## Conclusion

You now have a complete KCP implementation integrated into sing-box! The protocol follows the same patterns as Hysteria2 and TUIC, making it maintainable and consistent with the rest of the codebase.

For questions or issues, refer to:
- `kcp/README.md` - User documentation
- `kcp/IMPLEMENTATION_NOTES.md` - Technical details
- `kcp/MIGRATION_GUIDE.md` - If migrating from old version

**Happy coding! 🚀**