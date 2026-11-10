package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// mailServer is an in-memory IMAP server on 127.0.0.1 over TLS, trusted through imapTLS.
type mailServer struct {
	port  string
	users map[string]*imapmemserver.User
	mem   *imapmemserver.Server
}

func newMailServer(t *testing.T) *mailServer {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	imapTLS = &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
	t.Cleanup(func() { imapTLS = nil })

	mem := imapmemserver.New()
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true, // the listener is already TLS
	})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return &mailServer{port: port, users: map[string]*imapmemserver.User{}, mem: mem}
}

func (m *mailServer) addUser(t *testing.T, name, password string, folders ...string) {
	u := imapmemserver.NewUser(name, password)
	for _, f := range append([]string{"INBOX"}, folders...) {
		if err := u.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	m.mem.AddUser(u)
	m.users[name] = u
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

func (m *mailServer) deliver(t *testing.T, user, folder, msg string) {
	t.Helper()
	msg = strings.ReplaceAll(msg, "\n", "\r\n")
	if _, err := m.users[user].Append(folder, literal{bytes.NewReader([]byte(msg))}, &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
}

func message(id, subject, contentType, body string) string {
	return fmt.Sprintf("From: Carla <carla@example.com>\nTo: ann@example.com\nSubject: %s\nMessage-ID: <%s@example.com>\n"+
		"Date: Tue, 10 Nov 2026 09:00:00 -0300\nMIME-Version: 1.0\nContent-Type: %s\n\n%s\n", subject, id, contentType, body)
}

func TestReadMail(t *testing.T) {
	latin1 := message("a", "=?ISO-8859-1?Q?Reuni=E3o_de_sexta?=", "text/plain; charset=ISO-8859-1", "Confirma a reuni\xe3o?")
	raw, sg := readMail([]byte(strings.ReplaceAll(latin1, "\n", "\r\n")))
	if sg.Title != "Reunião de sexta" || sg.Details != "Confirma a reunião?" || sg.ExternalID != "a@example.com" {
		t.Errorf("latin-1 mail: %+v", sg)
	}
	if !strings.HasPrefix(raw, `From: "Carla" <carla@example.com>`) || !strings.Contains(raw, "Subject: Reunião de sexta\n\nConfirma") {
		t.Errorf("text kept: %q", raw)
	}
	multipart := message("b", "Report", `multipart/mixed; boundary="X"`, `--X
Content-Type: multipart/alternative; boundary="Y"

--Y
Content-Type: text/html; charset=utf-8

<html><head><style>p{color:red}</style></head><body><p>Please   send the <b>Q3</b> report</p><p>by Friday</p><script>x()</script></body></html>
--Y--
--X
Content-Type: application/pdf
Content-Disposition: attachment; filename="q3.pdf"

%PDF-1.4
--X--`)
	if _, sg := readMail([]byte(strings.ReplaceAll(multipart, "\n", "\r\n"))); sg.Details != "Please send the Q3 report\n\nby Friday" {
		t.Errorf("html mail text: %q", sg.Details)
	}
}

func TestMailboxes(t *testing.T) {
	a := newApp(t, false)
	a.s.secrets = newSealer("test secret")
	ctx := context.Background()
	mail := newMailServer(t)
	mail.addUser(t, "ann@example.com", "right", "Work")
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	// the test database is shared: leave no items for other tests' inbox reader
	t.Cleanup(func() { testDB.Exec(ctx, `DELETE FROM task_sources WHERE user_id = $1`, ann.ID) })
	mail.deliver(t, "ann@example.com", "INBOX", message("old", "Old mail", "text/plain", "from before"))

	form := func(name, folder, password string) url.Values {
		return url.Values{"name": {name}, "host": {"127.0.0.1"}, "port": {mail.port}, "security": {"tls"},
			"username": {"ann@example.com"}, "password": {password}, "folder": {folder}}
	}
	if res := ann.post("/tasks/mailboxes", form("Personal", "", "wrong")); !strings.Contains(res.body, "login failed") {
		t.Fatalf("a wrong password should be reported: %d %.300s", res.code, res.body)
	}
	if res := ann.post("/tasks/mailboxes", form("Personal", "", "right")); res.code != http.StatusSeeOther {
		t.Fatalf("add mailbox: %d %.300s", res.code, res.body)
	}
	ann.post("/tasks/mailboxes", form("Work", "Work", "right")) // any number of mailboxes
	personal := dbInt(t, `SELECT id FROM task_sources WHERE user_id = $1 AND name = 'Personal'`, ann.ID)
	work := dbInt(t, `SELECT id FROM task_sources WHERE user_id = $1 AND name = 'Work'`, ann.ID)
	var stored []byte
	testDB.QueryRow(ctx, `SELECT password FROM imap_accounts WHERE source_id = $1`, personal).Scan(&stored)
	if len(stored) == 0 || bytes.Contains(stored, []byte("right")) {
		t.Error("the mailbox password should be stored encrypted")
	}

	mail.deliver(t, "ann@example.com", "INBOX", message("n1", "Renew the car insurance", "text/plain", "It expires on the 30th."))
	mail.deliver(t, "ann@example.com", "INBOX", message("n2", "Lunch?", "text/html", "<p>Lunch on <b>Thursday</b>?</p>"))
	mail.deliver(t, "ann@example.com", "Work", message("w1", "Review the budget", "text/plain", "Before the board meeting."))
	// the checker picks up mailboxes not checked for a while
	testDB.Exec(ctx, `UPDATE imap_accounts SET checked_at = now() - interval '10 minutes'`)
	if err := a.s.checkDueMailboxes(ctx); err != nil {
		t.Fatal(err)
	}
	titles := func(source int64) string {
		rows, _ := testDB.Query(ctx, `SELECT title || ' / ' || details FROM inbox_items WHERE source_id = $1 ORDER BY id`, source)
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return strings.Join(out, "; ")
	}
	if got := titles(personal); got != "Renew the car insurance / It expires on the 30th.; Lunch? / Lunch on Thursday?" {
		t.Errorf("Personal (old mail must be skipped): %s", got)
	}
	if got := titles(work); got != "Review the budget / Before the board meeting." {
		t.Errorf("Work: %s", got)
	}
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE parse_state = 'pending'`); n != 3 {
		t.Errorf("%d items waiting for the model, want 3", n)
	}

	// nothing changed in the mailbox: the messages are still unread
	acc, _ := a.s.loadMailAccount(ctx, personal)
	acc.Password = "right"
	c, _, err := openMailbox(acc)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{Flags: true}).Collect()
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if slices.Contains(m.Flags, imap.FlagSeen) {
			t.Errorf("message %d was marked as read", m.SeqNum)
		}
	}

	// checking again adds nothing; new mail is added once
	a.s.checkMailboxNow(ctx, personal)
	mail.deliver(t, "ann@example.com", "INBOX", message("n3", "Call the plumber", "text/plain", "The kitchen sink."))
	ann.post(fmt.Sprintf("/tasks/mailboxes/%d/check", personal), nil)
	a.s.checkMailboxNow(ctx, personal)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE source_id = $1`, personal); n != 3 {
		t.Errorf("%d items from Personal, want 3", n)
	}

	// editing: an empty password keeps the saved one and the mailbox's place; a bad one changes nothing
	if res := ann.post(fmt.Sprintf("/tasks/mailboxes/%d/edit", personal), form("Home", "", "")); res.code != http.StatusSeeOther {
		t.Errorf("edit keeping the password: %d %.300s", res.code, res.body)
	}
	if res := ann.post(fmt.Sprintf("/tasks/mailboxes/%d/edit", personal), form("Broken", "", "wrong")); !strings.Contains(res.body, "login failed") {
		t.Error("a wrong new password should be reported")
	}
	if n := dbInt(t, `SELECT count(*) FROM task_sources WHERE id = $1 AND name = 'Home'`, personal); n != 1 {
		t.Error("the failed edit changed the mailbox")
	}
	a.s.checkMailboxNow(ctx, personal)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE source_id = $1`, personal); n != 3 {
		t.Errorf("editing re-imported mail: %d items", n)
	}

	// a failing check is shown on the page
	testDB.Exec(ctx, `UPDATE imap_accounts SET host = '127.0.0.2' WHERE source_id = $1`, work)
	a.s.checkMailboxNow(ctx, work)
	if body := ann.get("/tasks/sources").body; !strings.Contains(body, "Last check failed") {
		t.Error("the sources page should show the failed check")
	}

	// mailboxes are private
	for _, p := range []string{"/edit", "/check"} {
		if res := ben.post(fmt.Sprintf("/tasks/mailboxes/%d%s", personal, p), form("Mine", "", "x")); res.code != http.StatusNotFound {
			t.Errorf("Ben POST %s on Ann's mailbox: %d", p, res.code)
		}
	}
	if strings.Contains(ben.get("/tasks/sources").body, "ann@example.com at") {
		t.Error("Ben sees Ann's mailboxes")
	}

	// without SECRET_KEY no password can be stored
	a.s.secrets = nil
	if res := ann.post("/tasks/mailboxes", form("Other", "", "right")); !strings.Contains(res.body, "SECRET_KEY") {
		t.Error("a mailbox was added without SECRET_KEY")
	}
}
