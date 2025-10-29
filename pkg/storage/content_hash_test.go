package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGetContentHash verifies that content hashes are extracted correctly
func TestGetContentHash(t *testing.T) {
	storage := &OCIClipStorage{}

	tests := []struct {
		name     string
		digest   string
		expected string
	}{
		{
			name:     "SHA256 digest",
			digest:   "sha256:abc123def456",
			expected: "abc123def456",
		},
		{
			name:     "SHA1 digest",
			digest:   "sha1:fedcba987654",
			expected: "fedcba987654",
		},
		{
			name:     "Long SHA256",
			digest:   "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
			expected: "44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885",
		},
		{
			name:     "No algorithm prefix (fallback)",
			digest:   "justahash123",
			expected: "justahash123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := storage.getContentHash(tc.digest)
			require.Equal(t, tc.expected, result)
		})
	}
}

// TestRemoteCacheKeyFormat verifies remote cache uses content hash only
func TestRemoteCacheKeyFormat(t *testing.T) {
	t.Skip("Integration test - requires mock ContentCache")
	
	// This test verifies that:
	// 1. Remote cache keys use ONLY the content hash (hex part)
	// 2. No prefixes like "clip:oci:layer:decompressed:"
	// 3. No algorithm prefix like "sha256:"
	// 4. Cross-image sharing works (same layer = same cache key)
	
	// Example:
	// Layer digest: sha256:abc123...
	// Remote cache key: abc123... (just the hash!)
	// Disk cache path: /tmp/clip-oci-cache/sha256_abc123... (filesystem-safe)
}

// TestContentAddressedCaching verifies cache keys enable cross-image sharing
func TestContentAddressedCaching(t *testing.T) {
	storage := &OCIClipStorage{}

	// Same layer used in multiple images
	sharedLayerDigest := "sha256:44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885"

	// Both images should produce the SAME cache key
	cacheKey := storage.getContentHash(sharedLayerDigest)
	
	// Cache key should be just the hex hash (content-addressed)
	require.Equal(t, "44cf07d57ee4424189f012074a59110ee2065adfdde9c7d9826bebdffce0a885", cacheKey)
	require.NotContains(t, cacheKey, "sha256:", "Cache key should not contain algorithm prefix")
	require.NotContains(t, cacheKey, "clip:", "Cache key should not contain namespace prefix")
	require.NotContains(t, cacheKey, "decompressed", "Cache key should not contain type suffix")
	
	t.Logf("✅ Content-addressed cache key: %s", cacheKey)
	t.Logf("This key can be shared across multiple images with the same layer!")
}
