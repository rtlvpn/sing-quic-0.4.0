package tuic

import (
	"sync"
	"sync/atomic"
	"time"
)

// BandwidthLimit configuration structure
type BandwidthLimit struct {
	Enabled         bool  `json:"enabled"`
	DefaultUpload   int64 `json:"default_upload"`   // bytes per second
	DefaultDownload int64 `json:"default_download"` // bytes per second
	DefaultBurst    int64 `json:"default_burst"`    // bytes
}

// UserBandwidthLimits defines per-user bandwidth limits
type UserBandwidthLimits struct {
	Upload   int64 `json:"upload"`   // bytes per second
	Download int64 `json:"download"` // bytes per second
	Burst    int64 `json:"burst"`    // bytes
}

// BandwidthStats tracks bandwidth usage statistics
type BandwidthStats struct {
	UploadBytes     int64 `json:"upload_bytes"`
	DownloadBytes   int64 `json:"download_bytes"`
	UploadPackets   int64 `json:"upload_packets"`
	DownloadPackets int64 `json:"download_packets"`
}

// TokenBucket implements a token bucket algorithm for rate limiting
type TokenBucket struct {
	capacity      int64 // Maximum tokens
	tokens        int64 // Current tokens (atomic for thread safety)
	refillRate    int64 // Tokens per second
	lastRefill    int64 // Last refill time (nanoseconds since epoch, atomic)
	burstCapacity int64 // Burst capacity
	burstTokens   int64 // Current burst tokens (atomic)
	mu            sync.RWMutex
}

// NewTokenBucket creates a new token bucket
func NewTokenBucket(capacity, refillRate, burstCapacity int64) *TokenBucket {
	now := time.Now().UnixNano()
	return &TokenBucket{
		capacity:      capacity,
		tokens:        capacity,
		refillRate:    refillRate,
		lastRefill:    now,
		burstCapacity: burstCapacity,
		burstTokens:   burstCapacity,
	}
}

// ConsumeTokens attempts to consume the specified number of tokens
// Returns true if successful, false if not enough tokens available
func (tb *TokenBucket) ConsumeTokens(tokens int64) bool {
	if tokens <= 0 {
		return true
	}

	tb.refill()

	// Try to consume from regular tokens first
	for {
		regularTokens := atomic.LoadInt64(&tb.tokens)
		if regularTokens >= tokens {
			if atomic.CompareAndSwapInt64(&tb.tokens, regularTokens, regularTokens-tokens) {
				return true
			}
			// CAS failed, retry
			continue
		}
		break
	}

	// Try to consume from burst tokens if available
	for {
		burstTokens := atomic.LoadInt64(&tb.burstTokens)
		if burstTokens >= tokens {
			if atomic.CompareAndSwapInt64(&tb.burstTokens, burstTokens, burstTokens-tokens) {
				return true
			}
			// CAS failed, retry
			continue
		}
		break
	}

	return false
}

// refill refills the token bucket based on elapsed time
func (tb *TokenBucket) refill() {
	now := time.Now().UnixNano()
	lastRefill := atomic.LoadInt64(&tb.lastRefill)

	// Check if another goroutine already refilled
	if !atomic.CompareAndSwapInt64(&tb.lastRefill, lastRefill, now) {
		return
	}

	elapsed := now - lastRefill
	if elapsed <= 0 {
		return
	}

	// Calculate tokens to add
	tokensToAdd := elapsed * tb.refillRate / 1e9 // Convert nanoseconds to seconds

	// Update regular tokens
	tb.mu.Lock()
	currentTokens := atomic.LoadInt64(&tb.tokens)
	newTokens := currentTokens + tokensToAdd
	if newTokens > tb.capacity {
		newTokens = tb.capacity
	}
	atomic.StoreInt64(&tb.tokens, newTokens)

	// Update burst tokens (refill at a slower rate)
	currentBurstTokens := atomic.LoadInt64(&tb.burstTokens)
	// Burst tokens refill at 1/10 the rate of regular tokens
	burstTokensToAdd := tokensToAdd / 10
	newBurstTokens := currentBurstTokens + burstTokensToAdd
	if newBurstTokens > tb.burstCapacity {
		newBurstTokens = tb.burstCapacity
	}
	atomic.StoreInt64(&tb.burstTokens, newBurstTokens)
	tb.mu.Unlock()
}

// GetAvailableTokens returns the current available tokens (regular + burst)
func (tb *TokenBucket) GetAvailableTokens() int64 {
	tb.refill()
	regularTokens := atomic.LoadInt64(&tb.tokens)
	burstTokens := atomic.LoadInt64(&tb.burstTokens)
	return regularTokens + burstTokens
}

// BandwidthLimiter manages bandwidth limiting for a user
type BandwidthLimiter struct {
	uploadBucket   *TokenBucket
	downloadBucket *TokenBucket
	limits         UserBandwidthLimits
	stats          BandwidthStats
	mu             sync.RWMutex
}

// NewBandwidthLimiter creates a new bandwidth limiter for a user
func NewBandwidthLimiter(limits UserBandwidthLimits) *BandwidthLimiter {
	return &BandwidthLimiter{
		uploadBucket:   NewTokenBucket(limits.Upload, limits.Upload, limits.Burst),
		downloadBucket: NewTokenBucket(limits.Download, limits.Download, limits.Burst),
		limits:         limits,
	}
}

// LimitUpload checks if upload should be limited
// Returns the allowed bytes and a delay if needed
func (bl *BandwidthLimiter) LimitUpload(bytes int64) (allowed int64, delay time.Duration) {
	// First try to consume from regular tokens
	if bl.uploadBucket.ConsumeTokens(bytes) {
		// Update statistics
		bl.mu.Lock()
		bl.stats.UploadBytes += bytes
		bl.stats.UploadPackets++
		bl.mu.Unlock()
		return bytes, 0
	}

	// Not enough regular tokens, check what's available
	available := bl.uploadBucket.GetAvailableTokens()
	if available <= 0 {
		// No tokens available, calculate wait time
		waitTime := time.Duration(bytes*1e9/bl.limits.Upload) * time.Nanosecond
		return 0, waitTime
	}

	// Consume what's available (should be less than requested)
	if available > bytes {
		available = bytes
	}
	bl.uploadBucket.ConsumeTokens(available)

	// Update statistics
	bl.mu.Lock()
	bl.stats.UploadBytes += available
	bl.stats.UploadPackets++
	bl.mu.Unlock()

	return available, 0
}

// LimitDownload checks if download should be limited
// Returns the allowed bytes and a delay if needed
func (bl *BandwidthLimiter) LimitDownload(bytes int64) (allowed int64, delay time.Duration) {
	// First try to consume from regular tokens
	if bl.downloadBucket.ConsumeTokens(bytes) {
		// Update statistics
		bl.mu.Lock()
		bl.stats.DownloadBytes += bytes
		bl.stats.DownloadPackets++
		bl.mu.Unlock()
		return bytes, 0
	}

	// Not enough regular tokens, check what's available
	available := bl.downloadBucket.GetAvailableTokens()
	if available <= 0 {
		// No tokens available, calculate wait time
		waitTime := time.Duration(bytes*1e9/bl.limits.Download) * time.Nanosecond
		return 0, waitTime
	}

	// Consume what's available (should be less than requested)
	if available > bytes {
		available = bytes
	}
	bl.downloadBucket.ConsumeTokens(available)

	// Update statistics
	bl.mu.Lock()
	bl.stats.DownloadBytes += available
	bl.stats.DownloadPackets++
	bl.mu.Unlock()

	return available, 0
}

// GetStats returns current bandwidth statistics
func (bl *BandwidthLimiter) GetStats() BandwidthStats {
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	return bl.stats
}

// ResetStats resets the bandwidth statistics
func (bl *BandwidthLimiter) ResetStats() {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.stats = BandwidthStats{}
}

// UpdateLimits updates the bandwidth limits
func (bl *BandwidthLimiter) UpdateLimits(limits UserBandwidthLimits) {
	bl.mu.Lock()
	defer bl.mu.Unlock()

	bl.limits = limits

	// Update buckets with new limits
	bl.uploadBucket = NewTokenBucket(limits.Upload, limits.Upload, limits.Burst)
	bl.downloadBucket = NewTokenBucket(limits.Download, limits.Download, limits.Burst)
}

// BandwidthManager manages bandwidth limiters for multiple users
type BandwidthManager struct {
	limiters map[interface{}]*BandwidthLimiter // Keyed by user identifier
	defaults UserBandwidthLimits
	config   BandwidthLimit
	mu       sync.RWMutex
}

// NewBandwidthManager creates a new bandwidth manager
func NewBandwidthManager(config BandwidthLimit) *BandwidthManager {
	defaults := UserBandwidthLimits{
		Upload:   config.DefaultUpload,
		Download: config.DefaultDownload,
		Burst:    config.DefaultBurst,
	}

	return &BandwidthManager{
		limiters: make(map[interface{}]*BandwidthLimiter),
		defaults: defaults,
		config:   config,
	}
}

// GetLimiter returns a bandwidth limiter for the specified user
func (bm *BandwidthManager) GetLimiter(user interface{}) *BandwidthLimiter {
	if !bm.config.Enabled {
		return nil
	}

	bm.mu.RLock()
	limiter, exists := bm.limiters[user]
	bm.mu.RUnlock()

	if exists {
		return limiter
	}

	// Create new limiter for user
	bm.mu.Lock()
	defer bm.mu.Unlock()

	// Check again in case another goroutine created it
	if limiter, exists := bm.limiters[user]; exists {
		return limiter
	}

	limiter = NewBandwidthLimiter(bm.defaults)
	bm.limiters[user] = limiter
	return limiter
}

// SetUserLimits sets specific limits for a user
func (bm *BandwidthManager) SetUserLimits(user interface{}, limits UserBandwidthLimits) {
	if !bm.config.Enabled {
		return
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if limiter, exists := bm.limiters[user]; exists {
		limiter.UpdateLimits(limits)
	} else {
		bm.limiters[user] = NewBandwidthLimiter(limits)
	}
}

// RemoveLimiter removes a bandwidth limiter for a user
func (bm *BandwidthManager) RemoveLimiter(user interface{}) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	delete(bm.limiters, user)
}

// GetAllStats returns statistics for all users
func (bm *BandwidthManager) GetAllStats() map[interface{}]BandwidthStats {
	bm.mu.RLock()
	defer bm.mu.RUnlock()

	stats := make(map[interface{}]BandwidthStats)
	for user, limiter := range bm.limiters {
		stats[user] = limiter.GetStats()
	}
	return stats
}
