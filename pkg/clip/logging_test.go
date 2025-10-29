package clip

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		expected zerolog.Level
		wantErr  bool
	}{
		{"debug", "debug", zerolog.DebugLevel, false},
		{"info", "info", zerolog.InfoLevel, false},
		{"warn", "warn", zerolog.WarnLevel, false},
		{"warning", "warning", zerolog.WarnLevel, false},
		{"error", "error", zerolog.ErrorLevel, false},
		{"disabled", "disabled", zerolog.Disabled, false},
		{"none", "none", zerolog.Disabled, false},
		{"off", "off", zerolog.Disabled, false},
		{"case insensitive", "DEBUG", zerolog.DebugLevel, false},
		{"invalid", "invalid", zerolog.InfoLevel, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SetLogLevel(tt.level)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "invalid log level")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, zerolog.GlobalLevel())
			}
		})
	}
}

func ExampleSetLogLevel() {
	// Enable debug logging to see detailed operation logs
	SetLogLevel("debug")

	// Use info logging for normal operation (default)
	SetLogLevel("info")

	// Disable all logging
	SetLogLevel("disabled")
}
