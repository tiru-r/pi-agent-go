package platform

import (
	"fmt"
	"runtime"
)

// OS returns the normalised OS name (darwin, linux, windows).
func OS() string {
	return runtime.GOOS
}

// Arch returns the normalised architecture (amd64, arm64).
func Arch() string {
	return runtime.GOARCH
}

// UserAgent returns the User-Agent string for HTTP requests.
func UserAgent(version string) string {
	return fmt.Sprintf("pi-agent/%s (%s/%s; Go %s)", version, OS(), Arch(), runtime.Version())
}

// IsMacOS returns true when running on macOS.
func IsMacOS() bool { return runtime.GOOS == "darwin" }

// IsLinux returns true when running on Linux.
func IsLinux() bool { return runtime.GOOS == "linux" }

// IsWindows returns true when running on Windows.
func IsWindows() bool { return runtime.GOOS == "windows" }

// SupportsColor returns true when the terminal likely supports ANSI colors.
func SupportsColor() bool {
	return !IsWindows()
}

// SupportsImages returns true when the terminal likely supports inline images
// (iTerm2 or Kitty protocol).
func SupportsImages() bool {
	return IsMacOS() || IsLinux()
}
