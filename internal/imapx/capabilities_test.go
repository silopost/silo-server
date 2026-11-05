package imapx

import "testing"

func TestParseCapabilities(t *testing.T) {
	c := ParseCapabilities("imap4rev1 idle Condstore QRESYNC UIDPLUS MOVE SPECIAL-USE LIST-STATUS COMPRESS=DEFLATE")
	for name, got := range map[string]bool{
		"idle": c.HasIdle(), "condstore": c.HasCondstore(), "qresync": c.HasQRESYNC(),
		"uidplus": c.HasUIDPlus(), "move": c.HasMove(), "special-use": c.HasSpecialUse(),
		"list-status": c.HasListStatus(), "raw": c.Has("compress=deflate"),
	} {
		if !got {
			t.Errorf("%s: want true", name)
		}
	}
	for name, got := range map[string]bool{
		"objectid": c.HasObjectID(), "notify": c.HasNotify(), "gmail": c.HasGmailExt(), "login-disabled": c.LoginDisabled(),
	} {
		if got {
			t.Errorf("%s: want false", name)
		}
	}
}

func TestCapabilitiesImplications(t *testing.T) {
	// QRESYNC implies CONDSTORE (RFC 7162).
	if c := ParseCapabilities("IMAP4rev1 QRESYNC"); !c.HasCondstore() {
		t.Error("QRESYNC should imply CONDSTORE")
	}
	// IMAP4rev2 mandates IDLE, MOVE, UIDPLUS but not CONDSTORE.
	c := ParseCapabilities("IMAP4rev2")
	if !c.HasIdle() || !c.HasMove() || !c.HasUIDPlus() || c.HasCondstore() {
		t.Error("IMAP4rev2 implication mismatch")
	}
	if c := ParseCapabilities("IMAP4rev1 LOGINDISABLED"); !c.LoginDisabled() || c.HasMove() {
		t.Error("LOGINDISABLED / no MOVE")
	}
}
