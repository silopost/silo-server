package imapx

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// Capabilities is the set of extensions a server advertises. Queries follow
// go-imap/v2 semantics, so capabilities implied by IMAP4rev2 (IDLE, MOVE,
// UIDPLUS, ...) are reported as present.
type Capabilities struct {
	set imap.CapSet
}

// NewCapabilities wraps a go-imap capability set.
func NewCapabilities(set imap.CapSet) Capabilities { return Capabilities{set: set.Copy()} }

// ParseCapabilities parses space-separated capability atoms, e.g. the text of
// a "* CAPABILITY ..." line without the prefix. Names are case-insensitive.
func ParseCapabilities(s string) Capabilities {
	set := make(imap.CapSet)
	for _, f := range strings.Fields(s) {
		set[canonCap(f)] = struct{}{}
	}
	return Capabilities{set: set}
}

// Has reports whether a capability (e.g. "COMPRESS=DEFLATE") is available.
func (c Capabilities) Has(name string) bool { return c.set.Has(canonCap(name)) }

// Names returns the literally advertised capabilities (unordered).
func (c Capabilities) Names() []string {
	out := make([]string, 0, len(c.set))
	for n := range c.set {
		out = append(out, string(n))
	}
	return out
}

// HasIdle reports whether IDLE is available.
func (c Capabilities) HasIdle() bool { return c.set.Has(imap.CapIdle) }

// HasMove reports whether MOVE is available.
func (c Capabilities) HasMove() bool { return c.set.Has(imap.CapMove) }

// HasUIDPlus reports whether UIDPLUS is available.
func (c Capabilities) HasUIDPlus() bool { return c.set.Has(imap.CapUIDPlus) }

// HasCondstore reports whether CONDSTORE is available.
func (c Capabilities) HasCondstore() bool { return c.set.Has(imap.CapCondStore) }

// HasQRESYNC reports whether QRESYNC is available.
func (c Capabilities) HasQRESYNC() bool { return c.set.Has(imap.CapQResync) }

// HasSpecialUse reports whether SPECIAL-USE is available.
func (c Capabilities) HasSpecialUse() bool { return c.set.Has(imap.CapSpecialUse) }

// HasCreateSpecialUse reports whether CREATE-SPECIAL-USE is available.
func (c Capabilities) HasCreateSpecialUse() bool {
	return c.set.Has(imap.CapCreateSpecialUse)
}

// HasListStatus reports whether LIST-STATUS is available.
func (c Capabilities) HasListStatus() bool { return c.set.Has(imap.CapListStatus) }

// HasESearch reports whether ESEARCH is available.
func (c Capabilities) HasESearch() bool { return c.set.Has(imap.CapESearch) }

// HasObjectID reports whether OBJECTID is available.
func (c Capabilities) HasObjectID() bool { return c.set.Has(imap.CapObjectID) }

// HasNotify reports whether NOTIFY is available.
func (c Capabilities) HasNotify() bool { return c.set.Has(imap.CapNotify) }

// HasBinary reports whether BINARY is available.
func (c Capabilities) HasBinary() bool { return c.set.Has(imap.CapBinary) }

// HasUTF8Accept reports whether UTF8=ACCEPT is available.
func (c Capabilities) HasUTF8Accept() bool { return c.set.Has(imap.CapUTF8Accept) }

// HasGmailExt reports whether X-GM-EXT-1 (Gmail extensions) is available.
func (c Capabilities) HasGmailExt() bool { return c.set.Has("X-GM-EXT-1") }

// LoginDisabled reports LOGINDISABLED (cleartext LOGIN refused until TLS).
func (c Capabilities) LoginDisabled() bool { return c.set.Has("LOGINDISABLED") }

// canonCap upper-cases a capability name; go-imap spells the IMAP4rev
// capabilities with a lower-case "rev".
func canonCap(s string) imap.Cap {
	u := strings.ToUpper(s)
	switch u {
	case "IMAP4REV1":
		return imap.CapIMAP4rev1
	case "IMAP4REV2":
		return imap.CapIMAP4rev2
	}
	return imap.Cap(u)
}
