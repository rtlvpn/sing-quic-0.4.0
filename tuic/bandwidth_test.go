package tuic

import (
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	// Create a token bucket with 1000 tokens capacity and 100 tokens/second refill rate
	bucket := NewTokenBucket(1000, 100, 2000)

	// Test initial capacity
	if bucket.GetAvailableTokens() != 3000 { // 1000 regular + 2000 burst
		t.Errorf("Expected initial capacity to be 3000, got %d", bucket.GetAvailableTokens())
	}

	// Test consuming tokens
	if !bucket.ConsumeTokens(500) {
		t.Error("Failed to consume 500 tokens")
	}

	if bucket.GetAvailableTokens() != 2500 {
		t.Errorf("Expected 2500 tokens remaining, got %d", bucket.GetAvailableTokens())
	}

	// Test consuming more than available
	if bucket.ConsumeTokens(3000) {
		t.Error("Should not be able to consume 3000 tokens")
	}

	// Wait for refill and test
	time.Sleep(100 * time.Millisecond)
	bucket.refill()

	// Should have some tokens refilled
	available := bucket.GetAvailableTokens()
	if available <= 2500 {
		t.Errorf("Expected tokens to be refilled, got %d", available)
	}
}

func TestBandwidthLimiter(t *testing.T) {
	limits := UserBandwidthLimits{
		Upload:   1000, // 1000 bytes/second
		Download: 2000, // 2000 bytes/second
		Burst:    5000, // 5000 bytes burst
	}

	limiter := NewBandwidthLimiter(limits)

	// Test upload limiting - first consume all regular tokens
	allowed, delay := limiter.LimitUpload(1000)
	if allowed != 1000 {
		t.Errorf("Expected 1000 bytes allowed on first upload, got %d", allowed)
	}

	// Now try to consume more than regular limit
	allowed, delay = limiter.LimitUpload(1500)
	if allowed > 1000 {
		t.Errorf("Upload limit not enforced, allowed %d bytes", allowed)
	}
	if delay == 0 && allowed < 1500 {
		t.Error("Expected non-zero delay when limiting upload")
	}

	// Test download limiting - first consume regular tokens
	allowed, delay = limiter.LimitDownload(2000)
	if allowed != 2000 {
		t.Errorf("Expected 2000 bytes allowed on first download, got %d", allowed)
	}

	// Now try to consume more than regular limit
	allowed, delay = limiter.LimitDownload(1500)
	if allowed > 2000 {
		t.Errorf("Download limit not enforced, allowed %d bytes", allowed)
	}

	// Test burst capability
	limiter2 := NewBandwidthLimiter(limits)
	// First consume up to regular limit
	limiter2.LimitUpload(1000)
	// Then consume from burst
	allowed, delay = limiter2.LimitUpload(3000)
	if allowed == 0 {
		t.Error("Burst capacity not available")
	}
	t.Logf("Burst allowed %d bytes with delay %v", allowed, delay)

	// Test statistics
	stats := limiter.GetStats()
	if stats.UploadBytes == 0 {
		t.Error("Upload statistics not recorded")
	}
	if stats.DownloadBytes == 0 {
		t.Error("Download statistics not recorded")
	}
}

func TestBandwidthManager(t *testing.T) {
	config := BandwidthLimit{
		Enabled:         true,
		DefaultUpload:   1000,
		DefaultDownload: 2000,
		DefaultBurst:    5000,
	}

	manager := NewBandwidthManager(config)

	// Test getting limiter for user
	user1 := "user1"
	limiter := manager.GetLimiter(user1)
	if limiter == nil {
		t.Error("Failed to get bandwidth limiter for user")
	}

	// Test setting user-specific limits
	userLimits := UserBandwidthLimits{
		Upload:   500,
		Download: 1000,
		Burst:    2500,
	}
	manager.SetUserLimits(user1, userLimits)

	// Get limiter again and verify it has updated limits
	limiter = manager.GetLimiter(user1)
	if limiter == nil {
		t.Error("Failed to get updated bandwidth limiter for user")
	}

	// Test statistics
	allStats := manager.GetAllStats()
	if len(allStats) == 0 {
		t.Error("No statistics available")
	}
}

func TestBandwidthManagerDisabled(t *testing.T) {
	config := BandwidthLimit{
		Enabled: false,
	}

	manager := NewBandwidthManager(config)

	// Test that limiter is nil when disabled
	user1 := "user1"
	limiter := manager.GetLimiter(user1)
	if limiter != nil {
		t.Error("Should not return limiter when bandwidth limiting is disabled")
	}
}

func TestBandwidthLimiterUpdateLimits(t *testing.T) {
	limits := UserBandwidthLimits{
		Upload:   1000,
		Download: 2000,
		Burst:    5000,
	}

	limiter := NewBandwidthLimiter(limits)

	// Consume all tokens at old limit
	limiter.LimitUpload(1000)

	// Update limits
	newLimits := UserBandwidthLimits{
		Upload:   500,
		Download: 1000,
		Burst:    2500,
	}
	limiter.UpdateLimits(newLimits)

	// Test that new limits are applied
	// First consume all tokens at new limit
	allowed, delay := limiter.LimitUpload(500)
	if allowed != 500 {
		t.Errorf("Expected 500 bytes allowed at new limit, got %d", allowed)
	}

	// Now try to exceed new limit
	allowed, delay = limiter.LimitUpload(600)
	if allowed > 500 {
		t.Errorf("New upload limit not applied, allowed %d bytes", allowed)
	}
	t.Logf("Updated limit: allowed %d bytes with delay %v", allowed, delay)
}
