// Package collab relays live edits between the editors of a note.
package collab

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"slices"
	"sync"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-time editing protocol. The server does not understand Yjs: it keeps an ordered
// log of updates per note, relays updates and awareness between clients, and lets a
// client replace the log with a snapshot of its full state (compaction).
//
// Every message starts with a type byte.
//
//	client -> server: update [yjs update], awareness [payload], snapshot [8-byte seq][yjs state]
//	server -> client: update [8-byte seq][yjs update], awareness [payload],
//	                  synced [1 if this client must seed the doc from the note JSON], compact,
//	                  ack [8-byte seq of the client's own update]
const (
	MsgUpdate    = 0
	MsgAwareness = 1
	MsgSnapshot  = 2
	MsgSynced    = 3
	MsgCompact   = 4
	MsgAck       = 5

	// ask a client for a snapshot every time the log grows by this many rows
	CompactEvery = 100
)

type Hub struct {
	mu    sync.Mutex
	rooms map[int64]*room
}

func (h *Hub) room(noteID int64) *room {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms == nil {
		h.rooms = map[int64]*room{}
	}
	if h.rooms[noteID] == nil {
		h.rooms[noteID] = &room{noteID: noteID, clients: map[*client]bool{}}
	}
	return h.rooms[noteID]
}

// Kick disconnects all of a user's sockets, e.g. after their access changed.
// They reconnect with their new role, or are refused.
func (h *Hub) Kick(userID int64) {
	h.disconnect(func(*room) bool { return true }, func(c *client) bool { return c.userID == userID })
}

// KickNotes disconnects everyone editing the given notes, e.g. after a move changed who may
// access them.
func (h *Hub) KickNotes(noteIDs []int64) {
	h.disconnect(func(rm *room) bool { return slices.Contains(noteIDs, rm.noteID) }, func(*client) bool { return true })
}

func (h *Hub) disconnect(inRoom func(*room) bool, match func(*client) bool) {
	h.mu.Lock()
	var rooms []*room
	for _, rm := range h.rooms {
		if inRoom(rm) {
			rooms = append(rooms, rm)
		}
	}
	h.mu.Unlock()
	for _, rm := range rooms {
		rm.mu.Lock()
		for c := range rm.clients {
			if match(c) {
				rm.drop(c)
			}
		}
		rm.mu.Unlock()
	}
}

type client struct {
	userID    int64
	name      string // shown to others; the server sets it, so nobody can pose as someone else
	color     string
	clientID  uint64 // the Yjs client ID this connection speaks for, learned from its first awareness
	knowsID   bool
	send      chan []byte
	canEdit   bool
	closed    bool   // guarded by room.mu
	awareness []byte // last awareness message, replayed to clients that join later
}

// room serializes all writes for one note, so clients receive updates in seq order.
type room struct {
	mu      sync.Mutex
	noteID  int64
	rows    int
	clients map[*client]bool
	seeder  *client
}

func (rm *room) drop(c *client) {
	if !c.closed {
		c.closed = true
		delete(rm.clients, c)
		close(c.send)
	}
	if rm.seeder == c {
		rm.seeder = nil
	}
}

func (rm *room) sendTo(c *client, msg []byte) {
	if c.closed {
		return
	}
	select {
	case c.send <- msg:
	default: // too slow; it will resync on reconnect
		rm.drop(c)
	}
}

func (rm *room) broadcast(from *client, msg []byte) {
	for c := range rm.clients {
		if c != from {
			rm.sendTo(c, msg)
		}
	}
}

func seqMsg(typ byte, seq int64, data []byte) []byte {
	msg := make([]byte, 9+len(data))
	msg[0] = typ
	binary.BigEndian.PutUint64(msg[1:9], uint64(seq))
	copy(msg[9:], data)
	return msg
}

// join sends the whole update log to c, then registers it for broadcasts.
func (rm *room) join(ctx context.Context, db *pgxpool.Pool, c *client) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rows, err := db.Query(ctx, `SELECT seq, data FROM note_updates WHERE note_id = $1 ORDER BY seq`, rm.noteID)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var seq int64
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			return err
		}
		c.send <- seqMsg(MsgUpdate, seq, data)
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rm.rows = n
	seed := n == 0 && rm.seeder == nil && c.canEdit
	if seed {
		rm.seeder = c
	}
	synced := []byte{MsgSynced, 0}
	if seed {
		synced[1] = 1
	}
	c.send <- synced
	for other := range rm.clients {
		if other.awareness != nil {
			c.send <- other.awareness
		}
	}
	rm.clients[c] = true
	return nil
}

func (rm *room) leave(c *client) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.drop(c)
	if gone := awarenessGone(c.awareness); gone != nil {
		rm.broadcast(c, gone)
	}
}

// awarenessGone turns a client's last awareness message into one that marks it offline, so
// others drop its cursor right away instead of after the awareness timeout. The payload is
// lib0-encoded (same varints as encoding/binary): count, then clientID, clock, JSON state.
func awarenessGone(msg []byte) []byte {
	if len(msg) < 2 {
		return nil
	}
	b := msg[1:]
	var fields [3]uint64 // count, clientID, clock
	for i := range fields {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return nil
		}
		fields[i], b = v, b[n:]
	}
	if fields[0] != 1 {
		return nil
	}
	out := binary.AppendUvarint([]byte{MsgAwareness}, 1)
	out = binary.AppendUvarint(out, fields[1])
	out = binary.AppendUvarint(out, fields[2]+1)
	out = binary.AppendUvarint(out, 4)
	return append(out, "null"...)
}

// ownAwareness keeps only the awareness entry of the connection's own Yjs client and replaces
// its "user" field with the authenticated name and color. Without this, a client could show
// up as someone else, or hide or move other people's cursors. It returns nil when nothing is
// left to relay. The payload is lib0-encoded: count, then per client its ID, clock and JSON.
func (c *client) ownAwareness(msg []byte) []byte {
	b := msg[1:]
	next := func() (uint64, bool) {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return 0, false
		}
		b = b[n:]
		return v, true
	}
	count, ok := next()
	if !ok {
		return nil
	}
	var entries []byte
	kept := uint64(0)
	for range count {
		id, ok1 := next()
		clock, ok2 := next()
		size, ok3 := next()
		if !ok1 || !ok2 || !ok3 || size > uint64(len(b)) {
			return nil
		}
		state := b[:size]
		b = b[size:]
		if !c.knowsID {
			c.clientID, c.knowsID = id, true
		}
		if id != c.clientID {
			continue
		}
		if string(state) != "null" {
			var fields map[string]any
			if json.Unmarshal(state, &fields) != nil {
				return nil
			}
			fields["user"] = map[string]string{"name": c.name, "color": c.color}
			state, _ = json.Marshal(fields)
		}
		entries = binary.AppendUvarint(entries, id)
		entries = binary.AppendUvarint(entries, clock)
		entries = binary.AppendUvarint(entries, uint64(len(state)))
		entries = append(entries, state...)
		kept++
	}
	if kept == 0 {
		return nil
	}
	return append(binary.AppendUvarint([]byte{MsgAwareness}, kept), entries...)
}

func (rm *room) handle(ctx context.Context, db *pgxpool.Pool, c *client, msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	switch msg[0] {
	case MsgAwareness:
		if msg = c.ownAwareness(msg); msg != nil {
			c.awareness = msg
			rm.broadcast(c, msg)
		}
	case MsgUpdate:
		if !c.canEdit {
			return nil
		}
		var seq int64
		if err := db.QueryRow(ctx, `INSERT INTO note_updates (note_id, data) VALUES ($1, $2) RETURNING seq`,
			rm.noteID, msg[1:]).Scan(&seq); err != nil {
			return err
		}
		rm.rows++
		rm.seeder = nil
		rm.broadcast(c, seqMsg(MsgUpdate, seq, msg[1:]))
		rm.sendTo(c, seqMsg(MsgAck, seq, nil))
		if rm.rows%CompactEvery == 0 {
			rm.sendTo(c, []byte{MsgCompact})
		}
	case MsgSnapshot:
		if !c.canEdit || len(msg) < 9 {
			return nil
		}
		// The client has applied (or authored) every update up to seq.
		upTo := int64(binary.BigEndian.Uint64(msg[1:9]))
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		tag, err := tx.Exec(ctx, `DELETE FROM note_updates WHERE note_id = $1 AND seq <= $2`, rm.noteID, upTo)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO note_updates (note_id, data) VALUES ($1, $2)`, rm.noteID, msg[9:]); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		rm.rows += 1 - int(tag.RowsAffected())
	}
	return nil
}

// Serve runs one editor's connection to a note until it closes.
// name and color are what other editors see for this user.
func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn, db *pgxpool.Pool, noteID, userID int64, name, color string, canEdit bool) {
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &client{userID: userID, name: name, color: color, send: make(chan []byte, 256), canEdit: canEdit}
	go func() {
		defer cancel()
		for msg := range c.send {
			if err := conn.Write(ctx, websocket.MessageBinary, msg); err != nil {
				return
			}
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	rm := h.room(noteID)
	if err := rm.join(ctx, db, c); err != nil {
		log.Printf("note %d join: %v", noteID, err)
		rm.leave(c)
		return
	}
	defer rm.leave(c)
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err := rm.handle(ctx, db, c, msg); err != nil {
			log.Printf("note %d: %v", noteID, err)
			return
		}
	}
}
