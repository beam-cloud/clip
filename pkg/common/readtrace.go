package common

import (
	"context"
	"time"
)

type readTraceCallerPIDKey struct{}

// ReadTraceEvent describes a lazy read operation inside a mounted CLIP image.
type ReadTraceEvent struct {
	Operation        string
	Source           string
	Path             string
	LayerDigest      string
	DecompressedHash string
	Offset           int64
	Length           int64
	BytesRead        int64
	StartedAt        time.Time
	Duration         time.Duration
	CallerPID        uint32
	Success          bool
	Error            string
	Attrs            map[string]string
}

type ReadTraceObserver func(ReadTraceEvent)

func WithReadTraceCallerPID(ctx context.Context, pid uint32) context.Context {
	if pid == 0 {
		return ctx
	}
	return context.WithValue(ctx, readTraceCallerPIDKey{}, pid)
}

func ReadTraceCallerPID(ctx context.Context) uint32 {
	if ctx == nil {
		return 0
	}
	if pid, ok := ctx.Value(readTraceCallerPIDKey{}).(uint32); ok {
		return pid
	}
	return 0
}
