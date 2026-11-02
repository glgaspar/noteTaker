package main

import (
	"context"
	"encoding/binary"
	"log"
	"net/http"
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
	msgUpdate    = 0
	msgAwareness = 1
	msgSnapshot  = 2
	msgSynced    = 3
	msgCompact   = 4
	msgAck       = 5

	// ask a client for a snapshot every time the log grows by this many rows
	compactEvery = 100
)

type hub struct {
	mu    sync.Mutex
	rooms map[int64]*room
}

func (h *hub) room(noteID int64) *room {
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

// kick disconnects all of a user's sockets, e.g. after their access changed.
// They reconnect with their new role, or are refused.
func (h *hub) kick(userID int64) {
	h.mu.Lock()
	rooms := make([]*room, 0, len(h.rooms))
	for _, rm := range h.rooms {
		rooms = append(rooms, rm)
	}
	h.mu.Unlock()
	for _, rm := range rooms {
		rm.mu.Lock()
		for c := range rm.clients {
			if c.userID == userID {
				rm.drop(c)
			}
		}
		rm.mu.Unlock()
	}
}

type client struct {
	userID    int64
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
		c.send <- seqMsg(msgUpdate, seq, data)
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
	synced := []byte{msgSynced, 0}
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
	out := binary.AppendUvarint([]byte{msgAwareness}, 1)
	out = binary.AppendUvarint(out, fields[1])
	out = binary.AppendUvarint(out, fields[2]+1)
	out = binary.AppendUvarint(out, 4)
	return append(out, "null"...)
}

func (rm *room) handle(ctx context.Context, db *pgxpool.Pool, c *client, msg []byte) error {
	if len(msg) == 0 {
		return nil
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	switch msg[0] {
	case msgAwareness:
		c.awareness = msg
		rm.broadcast(c, msg)
	case msgUpdate:
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
		rm.broadcast(c, seqMsg(msgUpdate, seq, msg[1:]))
		rm.sendTo(c, seqMsg(msgAck, seq, nil))
		if rm.rows%compactEvery == 0 {
			rm.sendTo(c, []byte{msgCompact})
		}
	case msgSnapshot:
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

func (s *server) noteSocket(w http.ResponseWriter, r *http.Request) {
	noteID := pathID(r, "id")
	ws, err := s.roleOf(r.Context(), currentUser(r).ID, noteRole, noteID)
	if err != nil {
		serverError(w, err)
		return
	}
	if ws.Role == "" {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil) // rejects cross-origin requests
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := &client{userID: currentUser(r).ID, send: make(chan []byte, 256), canEdit: ws.CanEdit()}
	go func() {
		defer cancel()
		for msg := range c.send {
			if err := conn.Write(ctx, websocket.MessageBinary, msg); err != nil {
				return
			}
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	rm := s.hub.room(noteID)
	if err := rm.join(ctx, s.db, c); err != nil {
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
		if err := rm.handle(ctx, s.db, c, msg); err != nil {
			log.Printf("note %d: %v", noteID, err)
			return
		}
	}
}
