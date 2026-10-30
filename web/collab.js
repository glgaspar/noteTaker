import * as Y from "yjs";
import { Awareness, applyAwarenessUpdate, encodeAwarenessUpdate, removeAwarenessStates } from "y-protocols/awareness";

// Message types; see collab.go for the protocol.
const UPDATE = 0, AWARENESS = 1, SNAPSHOT = 2, SYNCED = 3, COMPACT = 4, ACK = 5;

function frame(type, data, seq) {
  const head = seq === undefined ? 1 : 9;
  const msg = new Uint8Array(head + data.length);
  msg[0] = type;
  if (seq !== undefined) new DataView(msg.buffer).setBigUint64(1, seq);
  msg.set(data, head);
  return msg;
}

// NoteProvider syncs a Y.Doc with the server over /notes/{id}/ws and reconnects on failure.
// onSynced(seed) runs after every (re)connect; seed is true when this client must fill an
// empty doc from the note's stored JSON. onStatus receives "online" or "offline".
export class NoteProvider {
  constructor(noteId, doc, { onSynced, onStatus }) {
    this.url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/notes/${noteId}/ws`;
    this.doc = doc;
    this.awareness = new Awareness(doc);
    this.onSynced = onSynced;
    this.onStatus = onStatus;
    this.lastSeq = 0n; // highest seq received or acked; snapshots cover everything up to it
    this.retry = 500;

    doc.on("update", (update, origin) => {
      if (origin !== this) this.send(UPDATE, update);
    });
    this.awareness.on("update", ({ added, updated, removed }, origin) => {
      if (origin === this) return;
      if (added.concat(updated, removed).includes(doc.clientID)) {
        this.send(AWARENESS, encodeAwarenessUpdate(this.awareness, [doc.clientID]));
      }
    });
    this.connect();
  }

  connect() {
    const ws = (this.ws = new WebSocket(this.url));
    ws.binaryType = "arraybuffer";
    this.synced = false;
    this.received = [];
    ws.onmessage = (e) => this.receive(new Uint8Array(e.data));
    ws.onclose = () => {
      if (this.destroyed) return;
      this.onStatus("offline");
      const others = [...this.awareness.getStates().keys()].filter((id) => id !== this.doc.clientID);
      removeAwarenessStates(this.awareness, others, this);
      setTimeout(() => this.connect(), this.retry);
      this.retry = Math.min(this.retry * 2, 10000);
    };
  }

  receive(m) {
    switch (m[0]) {
      case UPDATE: {
        const seq = new DataView(m.buffer).getBigUint64(1);
        const update = m.subarray(9);
        if (seq > this.lastSeq) this.lastSeq = seq;
        if (!this.synced) this.received.push(update);
        Y.applyUpdate(this.doc, update, this);
        break;
      }
      case AWARENESS:
        applyAwarenessUpdate(this.awareness, m.subarray(1), this);
        break;
      case SYNCED: {
        this.synced = true;
        this.retry = 500;
        // Send whatever the server is missing, e.g. edits made while offline.
        const server = new Y.Doc();
        for (const u of this.received) Y.applyUpdate(server, u);
        if (!Y.equalSnapshots(Y.snapshot(server), Y.snapshot(this.doc))) {
          this.send(UPDATE, Y.encodeStateAsUpdate(this.doc, Y.encodeStateVector(server)));
        }
        server.destroy();
        this.received = null;
        this.send(AWARENESS, encodeAwarenessUpdate(this.awareness, [this.doc.clientID]));
        this.onStatus("online");
        this.onSynced(m[1] === 1);
        break;
      }
      case ACK: {
        const seq = new DataView(m.buffer).getBigUint64(1);
        if (seq > this.lastSeq) this.lastSeq = seq;
        break;
      }
      case COMPACT:
        this.ws.send(frame(SNAPSHOT, Y.encodeStateAsUpdate(this.doc), this.lastSeq));
        break;
    }
  }

  send(type, data) {
    // Updates made before sync are recovered by the "missing" diff above.
    if (this.ws.readyState === WebSocket.OPEN && (this.synced || type !== UPDATE)) this.ws.send(frame(type, data));
  }

  destroy() {
    this.destroyed = true;
    this.awareness.setLocalState(null);
    this.ws.close();
    this.awareness.destroy();
  }
}
