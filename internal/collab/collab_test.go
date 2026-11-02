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
