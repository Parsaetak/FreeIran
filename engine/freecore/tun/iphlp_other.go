//go:build !windows

package tun

// Observe refuses the first-party IP Helper seam on non-Windows
// platforms with the honest capability error: there are no Windows
// IP Helper facts to read, and a fake observation would poison every
// verdict built on it.
func Observe() (Observation, error) {
	return Observation{}, ErrUnsupportedPlatform
}
