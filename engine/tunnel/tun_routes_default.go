//go:build !windows

// tun_routes_default.go: non-Windows platforms have no native TUN
// route-observation layer (v0.11.4 ships the Windows IP Helper
// collectors; the TUN dataplane itself is Windows-only in this
// release — see tun_other.go). The seams fail closed so the
// activation gate can never mistake "unobserved" for "verified" if a
// future platform wires TUN without first wiring observation.
package tunnel

func init() {
	collectForwardRoutes = func() ([]observedRoute, error) {
		return nil, ErrTUNRouteObservationUnavailable
	}

	collectOwnerTCPSockets = func() ([]observedOwnerSocket, error) {
		return nil, ErrTUNRouteObservationUnavailable
	}
}
