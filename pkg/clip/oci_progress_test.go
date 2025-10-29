package clip

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestOCIIndexProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping OCI progress test in short mode")
	}

	ctx := context.Background()
	tempDir := t.TempDir()
	outputFile := tempDir + "/alpine.clip"

	// Create progress channel
	progressChan := make(chan OCIIndexProgress, 10)
	
	// Track progress updates
	var updates []OCIIndexProgress
	done := make(chan bool)
	
	go func() {
		for update := range progressChan {
			t.Logf("Progress: [%d/%d] %s - %s", 
				update.LayerIndex, update.TotalLayers, update.Stage, update.Message)
			updates = append(updates, update)
		}
		done <- true
	}()

	// Index with progress reporting
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:     "docker.io/library/alpine:3.18",
		OutputPath:   outputFile,
		ProgressChan: progressChan,
	})
	
	close(progressChan)
	<-done

	assert.NoError(t, err)
	assert.NotEmpty(t, updates, "Should have received progress updates")

	// Verify we got both starting and completed events
	hasStarting := false
	hasCompleted := false
	for _, update := range updates {
		if update.Stage == "starting" {
			hasStarting = true
			assert.NotEmpty(t, update.LayerDigest, "Starting update should have digest")
			assert.Greater(t, update.TotalLayers, 0, "Should have total layers")
		}
		if update.Stage == "completed" {
			hasCompleted = true
			assert.Greater(t, update.FilesIndexed, 0, "Completed update should have file count")
		}
	}

	assert.True(t, hasStarting, "Should have received 'starting' updates")
	assert.True(t, hasCompleted, "Should have received 'completed' updates")
}

func ExampleCreateFromOCIImage_withProgress() {
	ctx := context.Background()
	
	// Create a progress channel
	progressChan := make(chan OCIIndexProgress, 10)
	
	// Handle progress updates in a goroutine
	go func() {
		for update := range progressChan {
			switch update.Stage {
			case "starting":
				fmt.Printf("⏳ Processing layer %d/%d...\n", 
					update.LayerIndex, update.TotalLayers)
			case "completed":
				fmt.Printf("✓ Completed layer %d/%d (%d files indexed)\n",
					update.LayerIndex, update.TotalLayers, update.FilesIndexed)
			}
		}
	}()
	
	// Index the image with progress reporting
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:     "docker.io/library/alpine:latest",
		OutputPath:   "alpine.clip",
		ProgressChan: progressChan,
	})
	
	close(progressChan)
	
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	
	fmt.Println("✓ Indexing complete!")
}

// Example: Progress with timeout
func ExampleCreateFromOCIImage_progressWithTimeout() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	
	progressChan := make(chan OCIIndexProgress, 10)
	
	// Progress monitoring with timeout
	go func() {
		timeout := time.NewTimer(10 * time.Second)
		defer timeout.Stop()
		
		for {
			select {
			case update, ok := <-progressChan:
				if !ok {
					return
				}
				fmt.Printf("[%s] Layer %d/%d: %s\n", 
					update.Stage, update.LayerIndex, update.TotalLayers, update.Message)
				timeout.Reset(10 * time.Second)
				
			case <-timeout.C:
				fmt.Println("⚠️  No progress update for 10 seconds")
				timeout.Reset(10 * time.Second)
			}
		}
	}()
	
	err := CreateFromOCIImage(ctx, CreateFromOCIImageOptions{
		ImageRef:     "docker.io/library/ubuntu:22.04",
		OutputPath:   "ubuntu.clip",
		ProgressChan: progressChan,
	})
	
	close(progressChan)
	
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
