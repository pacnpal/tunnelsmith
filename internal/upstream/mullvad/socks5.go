package mullvad

import (
	"errors"
	"fmt"
	"regexp"
)

// SOCKS5DomainSuffix is the domain Mullvad publishes per-server SOCKS5
// proxies under. See ADR-004.
const SOCKS5DomainSuffix = ".relays.mullvad.net"

// SOCKS5Port is the port every Mullvad SOCKS5 proxy listens on inside the
// WireGuard tunnel.
const SOCKS5Port = 1080

// hostnameRe captures the trailing numeric segment of a Mullvad WireGuard
// relay hostname so the SOCKS5 derivation can splice "socks5-" before it.
//
// Examples that match: "al-tia-wg-003", "us-nyc-wg-301".
// The first capture is the prefix up to and including "-wg"; the second is
// the trailing 1+ digits.
var hostnameRe = regexp.MustCompile(`^(.+-wg)-(\d+)$`)

// ErrUnexpectedHostname is returned when SOCKS5Address is given a relay
// hostname that does not match Mullvad's published convention. Callers
// should skip the relay rather than guess.
var ErrUnexpectedHostname = errors.New("mullvad: relay hostname does not match expected pattern")

// SOCKS5Address derives the per-server multihop SOCKS5 endpoint for a
// Mullvad WireGuard relay hostname returned by the relays API.
//
// Per ADR-004 the transformation is `^(.+-wg)-(\d+)$` -> `$1-socks5-$2`,
// then suffix with `.relays.mullvad.net:1080`. Concretely,
// `al-tia-wg-003` becomes `al-tia-wg-socks5-003.relays.mullvad.net:1080`.
func SOCKS5Address(hostname string) (string, error) {
	m := hostnameRe.FindStringSubmatch(hostname)
	if m == nil {
		return "", fmt.Errorf("%w: %q", ErrUnexpectedHostname, hostname)
	}
	return fmt.Sprintf("%s-socks5-%s%s:%d", m[1], m[2], SOCKS5DomainSuffix, SOCKS5Port), nil
}
