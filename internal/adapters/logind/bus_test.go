package logind

import (
	"testing"

	"bnema.dev/zerobus"
)

func TestObjectPathValidation(t *testing.T) {
	for _, path := range []objectPath{"/", managerPath, testSession, "/a_1/B2"} {
		if !path.IsValid() {
			t.Errorf("valid path rejected: %q", path)
		}
	}
	for _, path := range []objectPath{"", "a", "/a/", "//a", "/a-b", "/é"} {
		if path.IsValid() {
			t.Errorf("invalid path accepted: %q", path)
		}
	}
}

// Msg deliberately exposes no body constructor. Empty bodies let us exercise
// truncation and signature rejection without a private or real bus.
func TestDecodeReplyRejectsTruncatedAndUnknownBodies(t *testing.T) {
	for _, sig := range []string{"s", "o", "v", "h", "b", "a{sv}"} {
		t.Run(sig, func(t *testing.T) {
			if _, err := decodeReply(&zerobus.Msg{Signature: sig}); err == nil {
				t.Fatal("invalid reply accepted")
			}
		})
	}
	body, err := decodeReply(&zerobus.Msg{})
	if err != nil || len(body) != 0 {
		t.Fatalf("empty reply: %v, %v", body, err)
	}
}

func TestDecodeSignalValidatesSignatureAndBody(t *testing.T) {
	for _, member := range []string{"Lock", "Unlock"} {
		m := &zerobus.Msg{Sender: testOwner, Path: string(testSession), Interface: sessionInterface, Member: member}
		s := decodeSignal(m)
		if s == nil {
			t.Fatal("empty session signal rejected")
		}
		if _, ok := parseSignal(s, testOwner, testSession); !ok {
			t.Fatal("decoded session signal rejected")
		}
	}
	for _, sig := range []string{"b", "s", "bb"} {
		m := &zerobus.Msg{Sender: testOwner, Path: string(managerPath), Interface: managerInterface, Member: "PrepareForSleep", Signature: sig}
		if decodeSignal(m) != nil {
			t.Fatalf("invalid signal body accepted: %q", sig)
		}
	}
}
