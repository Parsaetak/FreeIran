package system

// PortableMode reports whether the application is running from a
// portable deployment (v0.9.0 layout): a config directory or a
// portable.marker file sits next to the running executable. Both
// platform path implementations (paths_unix.go / paths_windows.go)
// consult the same condition when resolving the base directory, so
// this helper answers "is the deployment portable?" for diagnostics
// and the developer settings surface.
//
// The check is read-only and safe to call at any time.
func PortableMode() bool {
	return portableRoot() != ""
}
