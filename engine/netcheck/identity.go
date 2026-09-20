// identity.go implements the v0.9.8.5 Network Identity diagnostic
// (§6): one compact, fast, user-triggered answer to "who am I on
// this network right now" —
//
//	Local IP        192.168.x.x (interface en0)
//	Public IP       x.x.x.x
//	ISP / Network   <measured organization>
//	ASN             ASxxxx
//	Interface       <adapter name>
//
// Safety contract (§6.4 / §9):
//
//   - EXPLICIT user action only — never automatic at startup, never
//     inside ordinary UI rendering (the service layer enforces it);
//   - bounded timeouts, full cancellation;
//   - the local-IP discovery sends NO packets (connected UDP sockets
//     are route lookups on every supported platform);
//   - the public-IP lookup reuses the SAME bounded identity endpoints
//     the public_ip tool established (ipify + Cloudflare trace, 2 KiB
//     body caps);
//   - the ISP/ASN metadata lookup uses documented, keyless HTTPS
//     endpoints (ipinfo.io primary, ipwho.is fallback). The request
//     inherently reveals only the caller's public IP — the same fact
//     the public-IP check already measures. No FreeIran
//     configuration, credentials, proxy URLs or telemetry of any
//     kind are transmitted;
//   - metadata that is unavailable is reported as Unknown — never
//     fabricated;
//   - the lookup source is exposed in the structured result for
//     transparency.
package netcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// IdentityTimeout bounds the whole identity check (default 15s).
const IdentityTimeout = 15 * time.Second

// identityMetadataEndpoints is the bounded, documented metadata
// source set (primary first; one fallback).
var identityMetadataEndpoints = []string{
	"https://ipinfo.io/json", // documented free endpoint, no key
	"https://ipwho.is/",      // documented free endpoint, no key
}

// identityIPEndpoints mirrors the public_ip tool's set exactly —
// ONE public-IP implementation, reused (§6.2).
var identityIPEndpoints = []string{
	"https://api.ipify.org",
	"https://www.cloudflare.com/cdn-cgi/trace",
}

// LocalAddress is one active local address with its interface.
type LocalAddress struct {
	Address   string `json:"address"`
	Interface string `json:"interface,omitempty"`
	Family    string `json:"family"` // ipv4 | ipv6
}

// LocalIdentity is the measured local network identity.
type LocalIdentity struct {
	// PrimaryIPv4 is the route-relevant local IPv4 — the source
	// address a connection to the public Internet would use (empty
	// when no IPv4 default route exists).
	PrimaryIPv4 string `json:"primary_ipv4,omitempty"`

	// PrimaryIPv6 is the route-relevant global IPv6 (empty when no
	// IPv6 default route exists — never fabricated).
	PrimaryIPv6 string `json:"primary_ipv6,omitempty"`

	// Interface names the adapter that owns the primary address.
	Interface string `json:"interface,omitempty"`

	// Others lists additional active non-loopback addresses (bounded)
	// — the honest answer when several interfaces are up.
	Others []LocalAddress `json:"others,omitempty"`

	// Measured marks that at least one address was actually observed.
	Measured bool `json:"measured"`
}

// PublicIdentity is the measured public (exit) identity.
type PublicIdentity struct {
	// DirectIP is the public IPv4/IPv6 as seen on the DIRECT path.
	DirectIP string `json:"direct_ip,omitempty"`

	// TunnelIP is the public IP as seen THROUGH the active tunnel
	// (only measured when the caller supplied the tunnel dialer).
	TunnelIP string `json:"tunnel_ip,omitempty"`

	// TunnelMatch compares direct and tunnel identities (meaningful
	// only when both were measured).
	TunnelMatch bool `json:"tunnel_match,omitempty"`

	// Endpoint names the identity endpoint that answered (transparency).
	Endpoint string `json:"endpoint,omitempty"`
}

// NetworkMetadata is the measured ISP / network organization data.
// Unavailable fields stay empty and Available stays false — nothing
// is invented.
type NetworkMetadata struct {
	Organization string `json:"organization,omitempty"` // ISP / org
	ASN          string `json:"asn,omitempty"`          // "AS15169"
	Country      string `json:"country,omitempty"`
	Region       string `json:"region,omitempty"`
	Source       string `json:"source,omitempty"` // metadata endpoint used
	Available    bool   `json:"available"`
}

// IdentityReport is the complete Network Identity result.
type IdentityReport struct {
	Local      LocalIdentity   `json:"local"`
	Public     PublicIdentity  `json:"public"`
	Metadata   NetworkMetadata `json:"metadata"`
	Path       ToolPath        `json:"path"` // direct | tunneled
	Provider   string          `json:"provider,omitempty"`
	CheckedAt  time.Time       `json:"checked_at"`
	DurationMS int64           `json:"duration_ms"`
	Cancelled  bool            `json:"cancelled,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// IdentityOptions tune one identity check.
type IdentityOptions struct {
	// Timeout bounds the whole check (0 = IdentityTimeout).
	Timeout time.Duration

	// Dial routes the public-IP and metadata lookups through the
	// ACTIVE tunnel (nil = direct).
	Dial DialFunc

	// Provider labels the tunnel ("" = direct).
	Provider string

	// SkipMetadata disables the ISP/ASN lookup (local + public only).
	SkipMetadata bool
}

// RunNetworkIdentity executes ONE bounded identity check.
func RunNetworkIdentity(ctx context.Context, opts IdentityOptions) IdentityReport {
	started := time.Now().UTC()

	report := IdentityReport{
		Path:      PathDirect,
		CheckedAt: started,
	}

	if opts.Timeout <= 0 {
		opts.Timeout = IdentityTimeout
	}

	if opts.Dial != nil {
		report.Path = PathTunneled
		report.Provider = opts.Provider
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// --- Local identity: NO network traffic -------------------------
	report.Local = discoverLocalIdentity()

	// --- Public identity: bounded HTTPS through the selected path ----
	directDial := DialFunc(nil)

	// The direct exit IP is ALWAYS worth measuring — it answers "am I
	// behind the tunnel right now" — but only when the run itself is
	// direct (a tunneled run reports the tunnel's identity; the
	// public_ip tool remains the two-sided comparison surface).
	dial := opts.Dial
	if dial == nil {
		dial = directDial
	}

	exitIP, endpoint := discoverExitIPWithSource(ctx, identityIPEndpoints, dial)

	report.Public.DirectIP = exitIP
	report.Public.Endpoint = endpoint

	if report.Path == PathTunneled {
		report.Public.TunnelIP = exitIP

		// For transparency on a tunneled run, measure the direct exit
		// too when it is cheap to do so (bounded, same endpoints).
		if direct, _ := discoverExitIPWithSource(ctx, identityIPEndpoints, nil); direct != "" {
			report.Public.DirectIP = direct
			report.Public.TunnelMatch = direct == exitIP
		}
	}

	if ctx.Err() != nil {
		report.Cancelled = true
		report.Error = ctx.Err().Error()
		report.DurationMS = elapsedMS(started)

		return report
	}

	if exitIP == "" && report.Public.DirectIP == "" {
		report.Error = "no identity endpoint answered"
		report.DurationMS = elapsedMS(started)

		return report
	}

	// --- ISP / ASN metadata: documented, keyless, bounded ------------
	if !opts.SkipMetadata {
		report.Metadata = lookupNetworkMetadata(ctx, dial)
	}

	report.DurationMS = elapsedMS(started)

	return report
}

// discoverLocalIdentity measures the local network identity from the
// actual interface table: the route-relevant primary addresses plus
// the other active non-loopback addresses (bounded to 6).
func discoverLocalIdentity() LocalIdentity {
	identity := LocalIdentity{}

	primary4, err4 := probeLocalAddress(context.Background())
	if err4 == nil {
		identity.PrimaryIPv4 = primary4.Address

		if primary4.Interface != "" {
			identity.Interface = primary4.Interface
		}
	}

	primary6, err6 := probeLocalAddress6(context.Background())
	if err6 == nil {
		identity.PrimaryIPv6 = primary6.Address

		if identity.Interface == "" && primary6.Interface != "" {
			identity.Interface = primary6.Interface
		}
	}

	// Additional active non-loopback addresses — the honest "other
	// active local addresses" surface (§6.1).
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}

			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}

			for _, a := range addrs {
				var ip net.IP

				switch v := a.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}

				if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
					continue
				}

				family := "ipv4"
				if ip.To4() == nil {
					family = "ipv6"
				}

				addr := ip.String()
				if addr == identity.PrimaryIPv4 || addr == identity.PrimaryIPv6 {
					continue
				}

				if len(identity.Others) < 6 {
					identity.Others = append(identity.Others, LocalAddress{
						Address: addr, Interface: iface.Name, Family: family,
					})
				}
			}
		}
	}

	identity.Measured = identity.PrimaryIPv4 != "" || identity.PrimaryIPv6 != "" ||
		len(identity.Others) > 0

	return identity
}

// ipinfoResponse is the ipinfo.io/json shape (documented).
type ipinfoResponse struct {
	IP      string `json:"ip"`
	Org     string `json:"org"`
	Country string `json:"country"`
	Region  string `json:"region"`
}

// ipwhoisResponse is the ipwho.is shape (documented).
type ipwhoisResponse struct {
	IP         string `json:"ip"`
	Success    bool   `json:"success"`
	Country    string `json:"country"`
	Region     string `json:"region"`
	Connection *struct {
		ASN int    `json:"asn"`
		Org string `json:"org"`
		ISP string `json:"isp"`
	} `json:"connection"`
}

// lookupNetworkMetadata resolves the ISP / ASN metadata from the
// documented keyless sources (primary + one bounded fallback). The
// request follows the same path (direct or tunnel) as the exit-IP
// check so the metadata describes the reported exit. Malformed or
// unavailable answers leave Available=false — nothing is fabricated.
func lookupNetworkMetadata(ctx context.Context, dial DialFunc) NetworkMetadata {
	for _, endpoint := range identityMetadataEndpoints {
		if ctx.Err() != nil {
			break
		}

		body, err := boundedIdentityFetch(ctx, endpoint, dial)
		if err != nil {
			continue
		}

		if metadata := parseNetworkMetadata(body, endpoint); metadata.Available {
			return metadata
		}
	}

	return NetworkMetadata{}
}

// boundedIdentityFetch performs one bounded, disposable GET (2 KiB
// body cap, no redirects to preserve the source identity).
func boundedIdentityFetch(ctx context.Context, endpoint string, dial DialFunc) ([]byte, error) {
	transport := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 6 * time.Second,
	}

	if dial != nil {
		transport.DialContext = dial
	}

	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // identity sources answer directly
		},
	}

	defer client.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	request.Header.Set("User-Agent", "FreeIran-Tools/1.0")
	request.Header.Set("Accept", "application/json")

	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 2048))
}

// parseNetworkMetadata defensively parses one metadata body into the
// normalized shape. Unknown shapes yield Available=false.
func parseNetworkMetadata(body []byte, source string) NetworkMetadata {
	text := strings.TrimSpace(string(body))
	if text == "" || text[0] != '{' {
		return NetworkMetadata{}
	}

	// ipinfo.io/json: org carries "ASxxxx Organization".
	var ipinfo ipinfoResponse

	if err := json.Unmarshal(body, &ipinfo); err == nil && ipinfo.Org != "" {
		metadata := NetworkMetadata{
			Organization: ipinfo.Org,
			Country:      ipinfo.Country,
			Region:       ipinfo.Region,
			Source:       source,
		}

		if asn, org, ok := splitASNOrg(ipinfo.Org); ok {
			metadata.ASN = asn
			metadata.Organization = org
		}

		if metadata.Organization != "" {
			metadata.Available = true

			return metadata
		}
	}

	// ipwho.is: connection.{asn, org, isp}.
	var ipwhois ipwhoisResponse

	if err := json.Unmarshal(body, &ipwhois); err == nil && ipwhois.Success && ipwhois.Connection != nil {
		metadata := NetworkMetadata{
			Organization: firstNonEmpty(ipwhois.Connection.ISP, ipwhois.Connection.Org),
			Country:      ipwhois.Country,
			Region:       ipwhois.Region,
			Source:       source,
		}

		if ipwhois.Connection.ASN > 0 {
			metadata.ASN = fmt.Sprintf("AS%d", ipwhois.Connection.ASN)
		}

		if metadata.Organization != "" || metadata.ASN != "" {
			metadata.Available = true

			return metadata
		}
	}

	return NetworkMetadata{}
}

// splitASNOrg splits ipinfo's "AS15169 Google LLC" convention into
// its ASN and organization parts.
func splitASNOrg(org string) (asn, organization string, ok bool) {
	if len(org) < 2 || org[0] != 'A' || org[1] != 'S' {
		return "", org, false
	}

	parts := strings.SplitN(org, " ", 2)
	if len(parts) != 2 {
		return "", org, false
	}

	number := strings.TrimPrefix(parts[0], "AS")
	if number == parts[0] || number == "" {
		return "", org, false
	}

	for _, r := range number {
		if r < '0' || r > '9' {
			return "", org, false
		}
	}

	return parts[0], parts[1], true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// discoverExitIPWithSource mirrors discoverExitIP (tools_path.go)
// and additionally reports WHICH endpoint answered.
func discoverExitIPWithSource(ctx context.Context, endpoints []string, dial DialFunc) (string, string) {
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			return "", ""
		}

		transport := &http.Transport{
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 6 * time.Second,
		}

		if dial != nil {
			transport.DialContext = dial
		}

		client := &http.Client{
			Timeout:   8 * time.Second,
			Transport: transport,
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}

		request.Header.Set("User-Agent", "FreeIran-Tools/1.0")

		resp, err := client.Do(request)
		if err != nil {
			client.CloseIdleConnections()

			continue
		}

		// Cap the body: identity endpoints return well under 1 KiB.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		client.CloseIdleConnections()

		if ip := parseExitIP(body); ip != "" {
			return ip, endpoint
		}
	}

	return "", ""
}
