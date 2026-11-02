package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"notetaker/internal/collab"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The server treats Yjs updates as opaque bytes, so these tests send arbitrary payloads.

type socket struct {
	t    *testing.T
	conn *websocket.Conn
	in   chan []byte // closed when the connection ends
}

func (u *user) connect(noteID int64) *socket {
	u.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(u.base, "http") + fmt.Sprintf("/notes/%d/ws", noteID)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: u.http})
	if err != nil {
		u.t.Fatalf("dial: %v", err)
	}
	u.t.Cleanup(func() { conn.CloseNow() })
	// A Read cancelled by its context closes the connection, so read in the background and
	// let tests wait on the channel with their own timeouts.
	s := &socket{t: u.t, conn: conn, in: make(chan []byte, 1024)}
	go func() {
		defer close(s.in)
		for {
			_, msg, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			s.in <- msg
		}
	}()
	return s
}

func (s *socket) send(msg ...byte) {
	s.t.Helper()
	if err := s.conn.Write(context.Background(), websocket.MessageBinary, msg); err != nil {
		s.t.Fatal(err)
	}
}

// next returns the next message of the given type, skipping others; nil on timeout or close.
func (s *socket) next(typ byte, timeout time.Duration) []byte {
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-s.in:
			if !ok {
				return nil
			}
			if len(msg) > 0 && msg[0] == typ {
				return msg
			}
		case <-deadline:
			return nil
		}
	}
}

func (s *socket) must(typ byte) []byte {
	s.t.Helper()
	msg := s.next(typ, 3*time.Second)
	if msg == nil {
		s.t.Fatalf("no message of type %d", typ)
	}
	return msg
}

func seqOf(msg []byte) uint64 { return binary.BigEndian.Uint64(msg[1:9]) }

// awareness builds an awareness message for one client, as y-protocols encodes it.
func awareness(clientID, clock uint64, state string) []byte {
	msg := binary.AppendUvarint([]byte{collab.MsgAwareness}, 1)
	msg = binary.AppendUvarint(msg, clientID)
	msg = binary.AppendUvarint(msg, clock)
	msg = binary.AppendUvarint(msg, uint64(len(state)))
	return append(msg, state...)
}

func TestCollabProtocol(t *testing.T) {
	a := newApp(t, false)
	owner := a.user("Owner", false)
	editor, viewer := a.user("Editor", false), a.user("Viewer", false)
	ws := owner.workspace("W")
	owner.share(fmt.Sprintf("/w/%d/members", ws), editor.Email, "editor")
	owner.share(fmt.Sprintf("/w/%d/members", ws), viewer.Email, "viewer")
	note := owner.note(ws, 0, "N", "")
	rows := func() int64 { return dbInt(t, `SELECT count(*) FROM note_updates WHERE note_id = $1`, note) }

	// The first editor on an empty log is asked to seed it; nobody else is.
	o := owner.connect(note)
	if got := o.must(collab.MsgSynced); got[1] != 1 {
		t.Fatal("first editor was not asked to seed")
	}
	e := editor.connect(note)
	if got := e.must(collab.MsgSynced); got[1] != 0 {
		t.Fatal("second editor was asked to seed too")
	}

	// Updates are stored, relayed with their seq, and acknowledged to the sender.
	o.send(append([]byte{collab.MsgUpdate}, "u1"...)...)
	relayed := e.must(collab.MsgUpdate)
	if !bytes.Equal(relayed[9:], []byte("u1")) {
		t.Fatalf("relayed %q", relayed[9:])
	}
	if ack := o.must(collab.MsgAck); seqOf(ack) != seqOf(relayed) {
		t.Fatal("ack seq differs from relayed seq")
	}

	// Late joiners get the log first, plus the presence of people already there.
	o.send(awareness(7, 1, `{"user":{"name":"Owner"}}`)...)
	if got := e.must(collab.MsgAwareness); !bytes.Contains(got, []byte("Owner")) {
		t.Fatal("awareness not relayed")
	}
	v := viewer.connect(note)
	if got := v.must(collab.MsgUpdate); !bytes.Equal(got[9:], []byte("u1")) {
		t.Fatal("late joiner did not get the log")
	}
	v.must(collab.MsgSynced)
	if got := v.must(collab.MsgAwareness); !bytes.Contains(got, []byte("Owner")) {
		t.Fatal("late joiner did not get existing presence")
	}

	// Presence carries the real account name, whatever the client claims, and a client cannot
	// speak for another client's ID.
	e.send(awareness(9, 1, `{"user":{"name":"Owner"}}`)...)
	if got := o.must(collab.MsgAwareness); !bytes.Contains(got, []byte(`"name":"Editor"`)) || bytes.Contains(got, []byte(`"name":"Owner"`)) {
		t.Fatalf("spoofed presence relayed: %q", got)
	}
	e.send(awareness(7, 2, "null")...) // try to make the owner disappear
	if o.next(collab.MsgAwareness, 300*time.Millisecond) != nil {
		t.Fatal("presence for someone else's client was relayed")
	}

	// Viewers' updates are ignored.
	before := rows()
	v.send(append([]byte{collab.MsgUpdate}, "evil"...)...)
	if e.next(collab.MsgUpdate, 300*time.Millisecond) != nil || rows() != before {
		t.Fatal("viewer update was accepted")
	}

	// Leaving marks the client offline for everyone right away.
	o.conn.Close(websocket.StatusNormalClosure, "")
	if got := e.must(collab.MsgAwareness); !bytes.HasSuffix(got, []byte("null")) {
		t.Fatalf("no offline awareness after leaving: %q", got)
	}

	// After enough updates the writer is asked for a snapshot, which replaces the log.
	var last uint64
	for i := 0; ; i++ {
		e.send(append([]byte{collab.MsgUpdate}, fmt.Sprint("x", i)...)...)
		last = seqOf(e.must(collab.MsgAck))
		if e.next(collab.MsgCompact, 20*time.Millisecond) != nil {
			break
		}
		if i > collab.CompactEvery {
			t.Fatal("never asked to compact")
		}
	}
	snapshot := binary.BigEndian.AppendUint64([]byte{collab.MsgSnapshot}, last)
	e.send(append(snapshot, "STATE"...)...)
	time.Sleep(200 * time.Millisecond)
	if n := rows(); n != 1 {
		t.Fatalf("log has %d rows after compaction, want 1", n)
	}
	fresh := editor.connect(note)
	if got := fresh.must(collab.MsgUpdate); !bytes.Equal(got[9:], []byte("STATE")) {
		t.Fatalf("fresh client got %q, want the snapshot", got[9:])
	}
}

// Changing someone's access disconnects their open editors.
func TestAccessChangeDisconnects(t *testing.T) {
	a := newApp(t, false)
	owner := a.user("Owner", false)
	guest := a.user("Guest", false)
	ws := owner.workspace("W")
	f := owner.folder(ws, "F", 0)
	note := owner.note(ws, f, "N", "")
	owner.share(fmt.Sprintf("/folders/%d/share", f), guest.Email, "editor")
	g := guest.connect(note)
	g.must(collab.MsgSynced)

	owner.post(fmt.Sprintf("/notes/%d/move", note), map[string][]string{"folder_id": {""}}) // out of the share
	deadline := time.After(3 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-g.in:
		case <-deadline:
			t.Fatal("socket stayed open after the note left the share")
		}
	}
	if res := guest.get(fmt.Sprintf("/notes/%d/ws", note)); res.code != 404 {
		t.Errorf("guest can still reach the note: %d", res.code)
	}
}
