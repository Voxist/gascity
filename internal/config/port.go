package config

import (
	"fmt"
	"strconv"
	"strings"
)

// PortString is a TCP port number carried as a string.
//
// The string representation is what every consumer wants: the value is spliced
// into `BEADS_DOLT_SERVER_PORT=<port>` shell prefixes, joined with
// net.JoinHostPort, and compared against other string-typed port fields such as
// contract.ConfigState.DoltPort. An int would force a conversion at each of
// those sites and lose the "unset" signal that the empty string carries.
//
// A plain `string` field, however, only decodes the quoted TOML form. A port
// number's natural spelling is the bare integer, and BurntSushi/toml rejects
// `dolt_port = 9876` into a string destination outright — which Parse then
// reports as a whole-file failure, so one unquoted digit stops the city from
// loading at all. PortString accepts both spellings by implementing
// encoding.TextUnmarshaler, which the decoder feeds from TOML integers as
// readily as from TOML strings.
type PortString string

// String returns the port as its decimal text, or "" when unset.
func (p PortString) String() string { return string(p) }

// UnmarshalText decodes a port from either TOML spelling — `9876` or `"9876"` —
// and rejects anything that is not a port number. Validating here keeps the
// looser decoder from becoming a silent sink: a typo surfaces at config load,
// naming the file and key, instead of reaching the Dolt dialer as a connection
// error with no provenance. An empty value is the unset signal and is allowed.
func (p *PortString) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))
	if raw == "" {
		*p = ""
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("invalid port %q: must be a number between 1 and 65535", raw)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q: must be between 1 and 65535", raw)
	}
	*p = PortString(strconv.Itoa(n))
	return nil
}
