package clip

import (
	"context"
	"testing"
	"time"
)

// BenchmarkCheckpointIntervals tests different checkpoint intervals
func BenchmarkCheckpointIntervals(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	intervals := []int64{1, 2, 4, 8}
	
	for _, interval := range intervals {
		b.Run(string(rune(interval))+"MiB", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ctx := context.Background()
				tempDir := b.TempDir()
				
				err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
					ImageRef:      "docker.io/library/alpine:3.18",
					OutputPath:    tempDir + "/test.clip",
					CheckpointMiB: interval,
				})
				
				if err != nil {
					b.Fatalf("Failed to index: %v", err)
				}
			}
		})
	}
}

// TestCheckpointPerformance measures performance across different intervals
func TestCheckpointPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	intervals := []int64{1, 2, 4, 8}
	ctx := context.Background()
	
	t.Log("Testing checkpoint intervals on Alpine image:")
	for _, interval := range intervals {
		tempDir := t.TempDir()
		
		start := time.Now()
		err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
			ImageRef:      "docker.io/library/alpine:3.18",
			OutputPath:    tempDir + "/test.clip",
			CheckpointMiB: interval,
		})
		duration := time.Since(start)
		
		if err != nil {
			t.Fatalf("Failed with interval %d MiB: %v", interval, err)
		}
		
		t.Logf("Interval %2d MiB: %v", interval, duration)
	}
}
