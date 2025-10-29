# Logging and Progress Reporting

The CLIP library provides two ways to monitor operations:
1. **Structured Logging** - For debugging and operational logs
2. **Progress Channels** - For programmatic progress tracking during OCI indexing

## Table of Contents
- [Logging Configuration](#logging-configuration)
- [Progress Reporting](#progress-reporting)

---

## Logging Configuration

The CLIP library uses structured logging with [zerolog](https://github.com/rs/zerolog). You can control the verbosity of logs using the `SetLogLevel()` function.

### Quick Start

```go
import "github.com/beam-cloud/clip/pkg/clip"

// Enable detailed debug logs (shows file operations, cache hits/misses, etc.)
clip.SetLogLevel("debug")

// Use info level for normal operation (default)
clip.SetLogLevel("info")

// Disable all logs
clip.SetLogLevel("disabled")
```

## Available Log Levels

| Level | Description | Use Case |
|-------|-------------|----------|
| `"debug"` | Detailed operation logs | Troubleshooting, development |
| `"info"` | High-level operation logs | Normal operation (default) |
| `"warn"` | Warning messages | Production with warnings |
| `"error"` | Error messages only | Production, minimal logging |
| `"disabled"` | No logs | Silent operation |

## Example: Toggling Debug Logs

```go
package main

import (
    "context"
    "github.com/beam-cloud/clip/pkg/clip"
)

func main() {
    // Enable debug logging to see detailed operations
    clip.SetLogLevel("debug")
    
    // Create an OCI archive with detailed logs
    err := clip.CreateFromOCIImage(context.Background(), clip.CreateFromOCIImageOptions{
        ImageRef:   "docker.io/library/alpine:latest",
        OutputPath: "alpine.clip",
    })
    
    // Switch back to info level for normal operation
    clip.SetLogLevel("info")
}
```

## Log Output Examples

### Debug Level
```json
{"level":"debug","path":"/bin/busybox","size":816888,"uoff":3072,"message":"File"}
{"level":"debug","digest":"sha256:abc123...","offset":0,"length":40,"message":"disk cache hit"}
{"level":"info","message":"creating archive from /app to /tmp/app.clip"}
```

### Info Level (Default)
```json
{"level":"info","message":"creating archive from /app to /tmp/app.clip"}
{"level":"info","message":"archive created successfully"}
```

### Disabled
No log output.

## Custom Log Output

You can also configure the global logger to write to a custom destination:

```go
import (
    "os"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
)

// Write logs to a file
logFile, _ := os.OpenFile("clip.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
log.Logger = zerolog.New(logFile).With().Timestamp().Logger()

// Pretty print logs for development
log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
```

---

## Progress Reporting

During OCI image indexing, you can receive real-time progress updates via a channel. This is useful for building UIs, progress bars, or monitoring long-running operations.

### Basic Usage

```go
package main

import (
    "context"
    "fmt"
    "github.com/beam-cloud/clip/pkg/clip"
)

func main() {
    ctx := context.Background()
    
    // Create a buffered channel for progress updates
    progressChan := make(chan clip.OCIIndexProgress, 10)
    
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
    
    // Index with progress reporting
    err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
        ImageRef:     "docker.io/library/alpine:latest",
        OutputPath:   "alpine.clip",
        ProgressChan: progressChan,
    })
    
    close(progressChan) // Important: close when done
    
    if err != nil {
        fmt.Printf("Error: %v\n", err)
    }
}
```

### Progress Update Structure

```go
type OCIIndexProgress struct {
    LayerIndex    int    // Current layer (1-based)
    TotalLayers   int    // Total number of layers
    LayerDigest   string // SHA256 digest of current layer
    Stage         string // "starting" or "completed"
    FilesIndexed  int    // Total files indexed so far (only for "completed")
    Message       string // Human-readable message
}
```

### Example: Progress Bar

```go
import (
    "fmt"
    "github.com/beam-cloud/clip/pkg/clip"
)

func indexWithProgressBar(imageRef, outputPath string) error {
    progressChan := make(chan clip.OCIIndexProgress, 10)
    
    go func() {
        for update := range progressChan {
            // Calculate progress percentage
            percent := float64(update.LayerIndex) / float64(update.TotalLayers) * 100
            
            switch update.Stage {
            case "starting":
                fmt.Printf("\r[%-50s] %3.0f%% Layer %d/%d", 
                    progress(percent), percent, update.LayerIndex, update.TotalLayers)
            case "completed":
                fmt.Printf("\r[%-50s] %3.0f%% Layer %d/%d (%d files)\n", 
                    progress(percent), percent, update.LayerIndex, 
                    update.TotalLayers, update.FilesIndexed)
            }
        }
    }()
    
    err := clip.CreateFromOCIImage(context.Background(), clip.CreateFromOCIImageOptions{
        ImageRef:     imageRef,
        OutputPath:   outputPath,
        ProgressChan: progressChan,
    })
    
    close(progressChan)
    return err
}

func progress(percent float64) string {
    bars := int(percent / 2) // 50 character width
    result := ""
    for i := 0; i < 50; i++ {
        if i < bars {
            result += "="
        } else if i == bars {
            result += ">"
        } else {
            result += " "
        }
    }
    return result
}
```

### Example: With Timeout Detection

```go
import (
    "context"
    "fmt"
    "time"
    "github.com/beam-cloud/clip/pkg/clip"
)

func indexWithTimeout(imageRef, outputPath string) error {
    ctx := context.Background()
    progressChan := make(chan clip.OCIIndexProgress, 10)
    
    go func() {
        timeout := time.NewTimer(10 * time.Second)
        defer timeout.Stop()
        
        for {
            select {
            case update, ok := <-progressChan:
                if !ok {
                    return // Channel closed
                }
                fmt.Printf("[%s] %s\n", update.Stage, update.Message)
                timeout.Reset(10 * time.Second) // Reset on activity
                
            case <-timeout.C:
                fmt.Println("⚠️  No progress for 10 seconds - may be stuck")
                timeout.Reset(10 * time.Second)
            }
        }
    }()
    
    err := clip.CreateFromOCIImage(ctx, clip.CreateFromOCIImageOptions{
        ImageRef:     imageRef,
        OutputPath:   outputPath,
        ProgressChan: progressChan,
    })
    
    close(progressChan)
    return err
}
```

### Notes

- Progress updates are **optional** - if you don't provide a `ProgressChan`, indexing works normally
- The channel should be **buffered** to prevent blocking the indexing operation
- Always **close the channel** after the operation completes to avoid goroutine leaks
- For multi-layer images (like Ubuntu), you'll receive multiple `starting` and `completed` events
- Progress reporting works **only for OCI image indexing** currently
