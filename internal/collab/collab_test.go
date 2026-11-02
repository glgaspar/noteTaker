package collab

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// awareness builds an awareness message for one client, as y-protocols encodes it.
func awareness(clientID, clock uint64, state string) []byte {
	msg := binary.AppendUvarint([]byte{MsgAwareness}, 1)
	msg = binary.AppendUvarint(msg, clientID)
	msg = binary.AppendUvarint(msg, clock)
	msg = binary.AppendUvarint(msg, uint64(len(state)))
	return append(msg, state...)
}

func TestAwarenessGone(t *testing.T) {
	gone := awarenessGone(awareness(300, 5, `{"x":1}`))
	if !bytes.Equal(gone, awareness(300, 6, "null")) {
		t.Fatalf("got %v", gone)
	}
	if awarenessGone(nil) != nil || awarenessGone([]byte{MsgAwareness, 2}) != nil {
		t.Fatal("malformed input should give nil")
	}
}

func TestOwnAwareness(t *testing.T) {
	c := &client{name: "Real Name", color: "#2563eb"}
	got := c.ownAwareness(awareness(42, 3, `{"user":{"name":"Admin","color":"red; background:url(x)"},"cursor":1}`))
	want := awareness(42, 3, `{"cursor":1,"user":{"color":"#2563eb","name":"Real Name"}}`)
	if !bytes.Equal(got, want) {
		t.Fatalf("identity not replaced:\n got %q\nwant %q", got, want)
	}
	if c.ownAwareness(awareness(7, 1, `{"user":{}}`)) != nil {
		t.Error("awareness for another client's ID was relayed")
	}
	if got := c.ownAwareness(awareness(42, 4, "null")); !bytes.Equal(got, awareness(42, 4, "null")) {
		t.Error("going offline must pass through")
	}
	if c.ownAwareness([]byte{MsgAwareness, 1, 42}) != nil {
		t.Error("truncated message accepted")
	}
}
