//go:build !linux

package clip

// Keep the existing handle path on other operating systems.
const immutableOpenDirCapability = 0
