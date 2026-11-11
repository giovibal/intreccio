package intreccio

import (
	"errors"
	"fmt"
	"testing"

	"github.com/giovibal/intreccio/internal/wire"
)

// TestForwardErrorRoundTrip checks that every sentinel error keeps its identity
// and its message across a forward: tagged with its code on the member that
// ran the query, sent, and rebuilt on the member that asked.
func TestForwardErrorRoundTrip(t *testing.T) {
	trip := func(err error) error {
		t.Helper()
		var remote *wire.Error
		if !errors.As(toRemote(err), &remote) {
			remote = &wire.Error{Code: wire.CodeGeneric, Msg: err.Error()} // what the server sends for an untagged error
		}
		back, derr := wire.DecodeError(wire.EncodeError(remote))
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		return fromRemote(back)
	}
	seen := map[wire.Code]bool{}
	for _, s := range remoteSentinels {
		if seen[s.code] {
			t.Errorf("wire code %d is used twice", s.code)
		}
		seen[s.code] = true
		err := trip(fmt.Errorf("query: %w", s.err))
		if !errors.Is(err, s.err) || err.Error() != "query: "+s.err.Error() {
			t.Errorf("%v: round trip = %v", s.err, err)
		}
	}
	plain := trip(errors.New("parse error"))
	if plain.Error() != "parse error" {
		t.Errorf("plain error round trip = %v", plain)
	}
	for _, s := range remoteSentinels {
		if errors.Is(plain, s.err) {
			t.Errorf("a plain error came back as %v", s.err)
		}
	}
	// An error already carrying a code (a voter relaying the leader's answer)
	// is passed on as it is.
	relayed := &wire.Error{Code: wire.CodeConflict, Msg: "query: conflict"}
	if toRemote(relayed) != error(relayed) {
		t.Error("toRemote re-tagged an error that already had a code")
	}
}
