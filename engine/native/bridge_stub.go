//go:build !native_accel

package native

// nativeCompiled reports whether this binary was built with the C++
// acceleration layer linked in. Without the native_accel build tag the
// pure-Go implementations are always used.
const nativeCompiled = false

// hashBatchNative is only available in native_accel builds.
func hashBatchNative(data []byte, offsets []uint32, out []uint64) {
	panic("native: hashBatchNative called without native_accel build tag")
}

// crc32Native is only available in native_accel builds.
func crc32Native(seed uint32, data []byte) uint32 {
	panic("native: crc32Native called without native_accel build tag")
}

// scanURLsNative is only available in native_accel builds.
func scanURLsNative(text []byte, out []uint32) (int, []uint32) {
	panic("native: scanURLsNative called without native_accel build tag")
}
