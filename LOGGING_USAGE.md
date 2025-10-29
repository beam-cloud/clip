# Logging Configuration

The CLIP library uses structured logging with [zerolog](https://github.com/rs/zerolog). You can control the verbosity of logs using the `SetLogLevel()` function.

## Quick Start

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
