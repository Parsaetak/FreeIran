package system

import "runtime"

// goosCheck exposes the host OS to tests without importing runtime in
// every test file.
func goosCheck() string {
	return runtime.GOOS
}

func detectGOOS() string {
	return runtime.GOOS
}

func detectGOARCH() string {
	return runtime.GOARCH
}
