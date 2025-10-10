# TUIC Bandwidth Limiting

This document describes the bandwidth limiting feature implemented for TUIC protocol.

## Overview

The bandwidth limiting feature allows you to control the upload and download speeds for TUIC users on a per-user basis. It uses a token bucket algorithm to provide smooth traffic shaping with burst capability.

## Features

- **Per-user bandwidth limits**: Set different limits for different users
- **Separate upload/download limits**: Control each direction independently
- **Burst capability**: Allow temporary bursts beyond normal limits
- **Statistics tracking**: Monitor bandwidth usage per user
- **Graceful degradation**: Falls back to defaults when user-specific limits aren't provided

## Configuration

### Basic Configuration

Add a `bandwidth_limit` section to your TUIC inbound configuration:

```json
{
  "bandwidth_limit": {
    "enabled": true,
    "default_upload": 512000,    // bytes per second (500 KB/s)
    "default_download": 512000,  // bytes per second (500 KB/s)
    "default_burst": 41943040    // bytes (40 MB)
  }
}
```

### Per-User Configuration

Add bandwidth limits to individual user entries:

```json
{
  "users": [
    {
      "name": "premium-user",
      "uuid": "b1234567-e89f-4d51-aefb-d9bf7f899ea8",
      "password": "premiumPass123",
      "upload": 1048576,    // bytes per second (1 MB/s)
      "download": 1048576,  // bytes per second (1 MB/s)
      "burst": 83886080     // bytes (80 MB)
    }
  ]
}
```

## Parameters

### BandwidthLimit Section

- `enabled`: Enable or disable bandwidth limiting (boolean)
- `default_upload`: Default upload speed in bytes per second
- `default_download`: Default download speed in bytes per second  
- `default_burst`: Default burst capacity in bytes

### User Parameters

- `upload`: Upload speed limit in bytes per second
- `download`: Download speed limit in bytes per second
- `burst`: Burst capacity in bytes

## Usage Examples

### Different User Tiers

```json
{
  "bandwidth_limit": {
    "enabled": true,
    "default_upload": 256000,
    "default_download": 256000,
    "default_burst": 20971520
  },
  "users": [
    {
      "name": "free-user",
      "uuid": "a1234567-b89c-4d51-aefb-d9bf7f899ea8",
      "password": "freePass",
      // Uses defaults (256 KB/s upload/download, 20 MB burst)
    },
    {
      "name": "premium-user", 
      "uuid": "b2345678-c89d-4d51-aefb-d9bf7f899ea8",
      "password": "premiumPass",
      "upload": 1048576,    // 1 MB/s upload
      "download": 2097152,  // 2 MB/s download
      "burst": 104857600    // 100 MB burst
    },
    {
      "name": "enterprise-user",
      "uuid": "c3456789-d89e-4d51-aefb-d9bf7f899ea8", 
      "password": "enterprisePass",
      "upload": 5242880,    // 5 MB/s upload
      "download": 10485760, // 10 MB/s download
      "burst": 536870912    // 512 MB burst
    }
  ]
}
```

## Implementation Details

### Token Bucket Algorithm

The bandwidth limiter uses a token bucket algorithm with these characteristics:

- **Regular tokens**: Refill at the configured rate (upload/download)
- **Burst tokens**: Refill at 1/10 the rate of regular tokens
- **Capacity**: Maximum tokens that can be accumulated
- **Consumption**: Tokens are consumed for each byte transferred

### Bandwidth Enforcement

- **Upload limiting**: Applied when sending data to clients
- **Download limiting**: Applied when receiving data from clients
- **UDP limiting**: Applied to both packet directions
- **TCP limiting**: Applied to stream connections

### Statistics

The system tracks these statistics per user:

- Upload bytes transferred
- Download bytes transferred
- Upload packet count
- Download packet count

## API Usage

### Creating a Service with Bandwidth Limiting

```go
config := BandwidthLimit{
    Enabled:         true,
    DefaultUpload:   512000,
    DefaultDownload: 512000,
    DefaultBurst:    41943040,
}

options := ServiceOptions{
    Context:        context.Background(),
    Logger:         logger,
    TLSConfig:      tlsConfig,
    BandwidthLimit: config,
    // ... other options
}

service, err := NewService(options)
```

### Setting User Bandwidth Limits

```go
// Update users with bandwidth limits
users := []string{"user1", "user2"}
uuids := [][16]byte{{...}, {...}}
passwords := []string{"pass1", "pass2"}
bandwidthLimits := []UserBandwidthLimits{
    {Upload: 512000, Download: 512000, Burst: 41943040},
    {Upload: 1024000, Download: 1024000, Burst: 83886080},
}

service.UpdateUsersWithBandwidth(users, uuids, passwords, bandwidthLimits)
```

### Getting Bandwidth Statistics

```go
stats := service.GetBandwidthStats()
for user, userStats := range stats {
    fmt.Printf("User %v: Upload: %d bytes, Download: %d bytes\n", 
        user, userStats.UploadBytes, userStats.DownloadBytes)
}
```

## Performance Considerations

- **Minimal overhead**: When disabled, bandwidth limiting has no performance impact
- **Thread-safe**: All operations are safe for concurrent use
- **Memory efficient**: Uses atomic operations and minimal state per user
- **Accurate limiting**: Provides precise rate limiting without abrupt cutoffs

## Troubleshooting

### Common Issues

1. **Limits not applied**: Ensure `enabled: true` in configuration
2. **Too strict limits**: Check if burst capacity is adequate
3. **Performance issues**: Verify limits are reasonable for your use case

### Debug Information

Enable debug logging to monitor bandwidth limiting:

```json
{
  "log": {
    "level": "debug",
    "timestamp": true
  }
}
```

## Migration

To enable bandwidth limiting on an existing configuration:

1. Add the `bandwidth_limit` section with appropriate defaults
2. Optionally add per-user limits to user entries
3. Set `enabled: true` to activate limiting
4. Monitor statistics to verify limits are working as expected