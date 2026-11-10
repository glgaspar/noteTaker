package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	_ "github.com/emersion/go-message/charset" // decode ISO-8859-1, Windows-1252, ... mail
	"github.com/emersion/go-message/mail"
	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Mailboxes are task sources read over IMAP every few minutes. The folder is opened read-only
// and bodies are fetched with PEEK, so nothing in the mailbox changes (not even \Seen).

const (
	mailEvery      = 5 * time.Minute // how often each mailbox is checked
	mailTimeout    = 2 * time.Minute // for a whole check: connect, log in, fetch
	mailPerCheck   = 50              // messages per check; the rest wait for the next one
	mailMaxMessage = 512 << 10       // bytes of a message fetched
	mailMaxText    = 20000           // characters of its text kept
)

// imapTLS is the TLS configuration for mail servers; tests trust their own certificate.
var imapTLS *tls.Config

type mailAccount struct {
	SourceID, UserID int64
	Host             string
	Port             int
	Security         string // "tls" (usually port 993) or "starttls" (143); plain text is refused
	Username         string
	Password         string
	Folder           string
	UIDValidity      uint32
	LastUID          uint32
}

// openMailbox logs in and opens the folder read-only. The connection gets a deadline, so a
// stuck server can't hold the checker.
func openMailbox(a mailAccount) (*imapclient.Client, *imap.SelectData, error) {
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).Dial("tcp", net.JoinHostPort(a.Host, strconv.Itoa(a.Port)))
	if err != nil {
		return nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(mailTimeout))
	tlsConfig := imapTLS
	if tlsConfig == nil {
		tlsConfig = &tls.Config{ServerName: a.Host}
	}
	opts := &imapclient.Options{TLSConfig: tlsConfig}
	var c *imapclient.Client
	if a.Security == "starttls" {
		if c, err = imapclient.NewStartTLS(conn, opts); err != nil {
			conn.Close()
			return nil, nil, err
		}
	} else {
		c = imapclient.New(tls.Client(conn, tlsConfig), opts)
	}
	if err := c.Login(a.Username, a.Password).Wait(); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("login failed: %w", err)
	}
	sel, err := c.Select(a.Folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("can't open folder %q: %w", a.Folder, err)
	}
	return c, sel, nil
}

// checkMailbox adds the messages that arrived since the last check to the user's inbox.
// The first check, or one after the folder was recreated (new UIDVALIDITY), only notes where
// the mailbox is, so old mail isn't imported.
func (s *Server) checkMailbox(ctx context.Context, a mailAccount) error {
	c, sel, err := openMailbox(a)
	if err != nil {
		return err
	}
	defer c.Close()
	defer c.Logout()
	newest := uint32(sel.UIDNext) - 1
	if sel.UIDValidity != a.UIDValidity {
		return s.saveMailboxPlace(ctx, a.SourceID, sel.UIDValidity, newest)
	}
	if newest <= a.LastUID {
		return s.saveMailboxPlace(ctx, a.SourceID, sel.UIDValidity, a.LastUID)
	}
	// List the new messages' UIDs (cheap), then download only the oldest mailPerCheck of them,
	// so a backlog isn't fetched again on every check.
	var after imap.UIDSet
	after.AddRange(imap.UID(a.LastUID+1), 0) // 0 is "*"
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{after}}, nil).Wait()
	if err != nil {
		return err
	}
	// "n:*" also matches the newest message when its UID is below n, so filter.
	uids := slices.DeleteFunc(found.AllUIDs(), func(u imap.UID) bool { return uint32(u) <= a.LastUID })
	slices.Sort(uids)
	if len(uids) == 0 {
		return s.saveMailboxPlace(ctx, a.SourceID, sel.UIDValidity, a.LastUID)
	}
	uids = uids[:min(len(uids), mailPerCheck)]
	msgs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: mailMaxMessage}}},
	}).Collect()
	if err != nil {
		return err
	}
	slices.SortFunc(msgs, func(x, y *imapclient.FetchMessageBuffer) int { return int(x.UID) - int(y.UID) })
	last := a.LastUID
	for _, m := range msgs {
		var body []byte
		for _, b := range m.BodySection {
			body = b.Bytes
		}
		raw, sg := readMail(body)
		if err := s.addToInbox(ctx, a.UserID, a.SourceID, raw, sg); err != nil {
			if errors.Is(err, errInboxFull) {
				break // retried after the user handles some items
			}
			return err
		}
		last = uint32(m.UID)
	}
	return s.saveMailboxPlace(ctx, a.SourceID, sel.UIDValidity, last)
}

func (s *Server) saveMailboxPlace(ctx context.Context, sourceID int64, validity, lastUID uint32) error {
	_, err := s.db.Exec(ctx, `UPDATE imap_accounts SET uid_validity = $2, last_uid = $3 WHERE source_id = $1`,
		sourceID, int64(validity), int64(lastUID))
	return err
}

// readMail turns a message into the inbox item's text (headers and body) and a first guess
// at the task: the subject and the body.
func readMail(msg []byte) (string, suggestion) {
	r, err := mail.CreateReader(strings.NewReader(string(msg)))
	if err != nil && r == nil {
		text := strings.ToValidUTF8(string(msg), "")
		return text, suggest(text)
	}
	defer r.Close()
	subject, _ := r.Header.Subject()
	id, _ := r.Header.MessageID()
	from := ""
	if addrs, err := r.Header.AddressList("From"); err == nil && len(addrs) > 0 {
		from = addrs[0].String()
	}
	var plain, htmlText string
	for {
		p, err := r.NextPart()
		if err != nil {
			break // io.EOF, or a part we can't read: keep what we have
		}
		h, ok := p.Header.(*mail.InlineHeader)
		if !ok {
			continue // attachments
		}
		kind, _, _ := h.ContentType()
		data, _ := io.ReadAll(io.LimitReader(p.Body, mailMaxMessage))
		switch {
		case kind == "text/plain" && plain == "":
			plain = string(data)
		case kind == "text/html" && htmlText == "":
			htmlText = htmlToText(string(data))
		}
	}
	body := strings.TrimSpace(plain)
	if body == "" {
		body = strings.TrimSpace(htmlText)
	}
	if r := []rune(body); len(r) > mailMaxText {
		body = string(r[:mailMaxText]) + "\n[…cut]"
	}
	var head strings.Builder
	if from != "" {
		fmt.Fprintf(&head, "From: %s\n", from)
	}
	if date, err := r.Header.Date(); err == nil {
		fmt.Fprintf(&head, "Date: %s\n", date.Format(time.RFC1123Z))
	}
	fmt.Fprintf(&head, "Subject: %s\n\n", subject)
	title := strings.TrimSpace(subject)
	if title == "" {
		title, _, _ = strings.Cut(body, "\n")
	}
	return head.String() + body, suggestion{ExternalID: id, Title: title, Details: body}
}

// htmlToText keeps the text of an HTML mail, with line breaks at blocks.
func htmlToText(s string) string {
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skip := 0 // inside <script>, <style>, <head>
	for {
		switch tt := z.Next(); tt {
		case html.ErrorToken:
			lines := strings.Split(b.String(), "\n")
			for i := range lines {
				lines[i] = strings.Join(strings.Fields(lines[i]), " ")
			}
			out := strings.Join(slices.Compact(lines), "\n")
			return strings.TrimSpace(out)
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
			}
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch a := atom.Lookup(name); a {
			case atom.Script, atom.Style, atom.Head:
				if tt == html.StartTagToken {
					skip++
				} else if tt == html.EndTagToken && skip > 0 {
					skip--
				}
			case atom.Br, atom.P, atom.Div, atom.Tr, atom.Li, atom.H1, atom.H2, atom.H3, atom.H4, atom.Table, atom.Blockquote:
				b.WriteString("\n")
			}
		}
	}
}

// ---- the checker ----

// checkSourcesEvery checks the mailboxes and APIs that are due, one at a time.
func (s *Server) checkSourcesEvery(tick time.Duration) {
	for {
		if err := s.checkDueMailboxes(context.Background()); err != nil {
			log.Printf("mailboxes: %v", err)
		}
		if err := s.checkDueAPIs(context.Background()); err != nil {
			log.Printf("api sources: %v", err)
		}
		time.Sleep(tick)
	}
}

func (s *Server) checkDueMailboxes(ctx context.Context) error {
	rows, err := s.db.Query(ctx, `SELECT a.source_id FROM imap_accounts a
		WHERE a.checked_at IS NULL OR a.checked_at < now() - $1::interval ORDER BY a.checked_at NULLS FIRST`,
		fmt.Sprintf("%d seconds", int(mailEvery.Seconds())))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.checkMailboxNow(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// checkMailboxNow checks one mailbox and records when and how it went. Only database errors
// are returned; the mailbox's own are shown on the sources page.
func (s *Server) checkMailboxNow(ctx context.Context, sourceID int64) error {
	a, err := s.loadMailAccount(ctx, sourceID)
	if err != nil {
		return err
	}
	msg := ""
	if a.Password, err = s.secrets.open([]byte(a.Password)); err == nil {
		err = s.checkMailbox(ctx, a)
	}
	if err != nil {
		var dbErr interface{ SQLState() string }
		if errors.As(err, &dbErr) {
			return err
		}
		msg = err.Error()
	}
	_, err = s.db.Exec(ctx, `UPDATE imap_accounts SET checked_at = now(), last_error = $2 WHERE source_id = $1`, sourceID, msg)
	return err
}

// loadMailAccount reads a mailbox; its Password is still encrypted.
func (s *Server) loadMailAccount(ctx context.Context, sourceID int64) (mailAccount, error) {
	var a mailAccount
	var password []byte
	var validity, last int64
	err := s.db.QueryRow(ctx, `SELECT a.source_id, s.user_id, a.host, a.port, a.security, a.username, a.password, a.folder,
		a.uid_validity, a.last_uid FROM imap_accounts a JOIN task_sources s ON s.id = a.source_id WHERE a.source_id = $1`, sourceID).
		Scan(&a.SourceID, &a.UserID, &a.Host, &a.Port, &a.Security, &a.Username, &password, &a.Folder, &validity, &last)
	a.Password, a.UIDValidity, a.LastUID = string(password), uint32(validity), uint32(last)
	return a, err
}

// ---- adding, editing and checking mailboxes ----

// mailForm reads the mailbox form. An empty password keeps the saved one (when editing).
func mailForm(r *http.Request) (name string, a mailAccount, problem string) {
	name = strings.TrimSpace(r.FormValue("name"))
	a = mailAccount{Host: strings.TrimSpace(r.FormValue("host")), Security: r.FormValue("security"),
		Username: strings.TrimSpace(r.FormValue("username")), Password: r.FormValue("password"),
		Folder: strings.TrimSpace(r.FormValue("folder"))}
	a.Port, _ = strconv.Atoi(r.FormValue("port"))
	if a.Folder == "" {
		a.Folder = "INBOX"
	}
	if a.Security != "starttls" {
		a.Security = "tls"
	}
	if a.Port == 0 {
		a.Port = map[string]int{"tls": 993, "starttls": 143}[a.Security]
	}
	if name == "" {
		name = a.Username
	}
	switch {
	case a.Host == "" || a.Username == "":
		problem = "Enter the server and the username."
	case a.Port < 1 || a.Port > 65535:
		problem = "The port must be between 1 and 65535."
	}
	return name, a, problem
}

// mailboxError shows the sources page again with the form as it was sent.
func (s *Server) mailboxError(w http.ResponseWriter, r *http.Request, id int64, msg string) {
	s.renderSources(w, r, map[string]any{"MailError": msg, "MailFormID": id, "MailForm": map[string]string{
		"name": r.FormValue("name"), "host": r.FormValue("host"), "port": r.FormValue("port"), "security": r.FormValue("security"),
		"username": r.FormValue("username"), "folder": r.FormValue("folder"), "tags": r.FormValue("tags")}})
}

// tryMailbox logs in to check the settings, and returns where the mailbox is now.
func tryMailbox(a mailAccount) (validity, newest uint32, err error) {
	c, sel, err := openMailbox(a)
	if err != nil {
		return 0, 0, err
	}
	c.Logout()
	c.Close()
	return sel.UIDValidity, uint32(sel.UIDNext) - 1, nil
}

func (s *Server) addMailbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name, a, problem := mailForm(r)
	switch {
	case s.secrets == nil:
		problem = "Mailboxes can't be added: the server has no SECRET_KEY set."
	case problem == "" && a.Password == "":
		problem = "Enter the password (for Gmail and Outlook, an app password)."
	}
	if problem != "" {
		s.mailboxError(w, r, 0, problem)
		return
	}
	validity, newest, err := tryMailbox(a)
	if err != nil {
		s.mailboxError(w, r, 0, "Couldn't connect: "+err.Error())
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO task_sources (user_id, kind, name, tags) VALUES ($1, 'imap', $2, $3) RETURNING id`,
		currentUser(r).ID, name, formTags(r)).Scan(&id)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO imap_accounts (source_id, host, port, security, username, password, folder,
			uid_validity, last_uid, checked_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`,
			id, a.Host, a.Port, a.Security, a.Username, s.secrets.seal(a.Password), a.Folder, int64(validity), int64(newest))
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}

// ownMailbox loads the {id} mailbox if it is the current user's; otherwise it answers 404.
func (s *Server) ownMailbox(w http.ResponseWriter, r *http.Request) (mailAccount, bool) {
	a, err := s.loadMailAccount(r.Context(), pathID(r, "id"))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.UserID != currentUser(r).ID) {
		s.fail(w, r, http.StatusNotFound, "")
		return a, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return a, false
	}
	return a, true
}

func (s *Server) editMailbox(w http.ResponseWriter, r *http.Request) {
	old, ok := s.ownMailbox(w, r)
	if !ok {
		return
	}
	name, a, problem := mailForm(r)
	if problem != "" {
		s.mailboxError(w, r, old.SourceID, problem)
		return
	}
	sealed := []byte(old.Password) // the saved password, unless a new one was typed
	if a.Password != "" {
		if s.secrets == nil {
			s.mailboxError(w, r, old.SourceID, "Passwords can't be saved: the server has no SECRET_KEY set.")
			return
		}
		sealed = s.secrets.seal(a.Password)
	} else {
		var err error
		if a.Password, err = s.secrets.open(sealed); err != nil {
			s.mailboxError(w, r, old.SourceID, "Enter the password again: "+err.Error())
			return
		}
	}
	validity, newest, err := tryMailbox(a)
	if err != nil {
		s.mailboxError(w, r, old.SourceID, "Couldn't connect: "+err.Error())
		return
	}
	// The same mailbox carries on where it was; another one starts from now.
	if a.Host == old.Host && a.Username == old.Username && a.Folder == old.Folder && validity == old.UIDValidity {
		newest = old.LastUID
	}
	if _, err := s.db.Exec(r.Context(), `
		WITH s AS (UPDATE task_sources SET name = $2 WHERE id = $1)
		UPDATE imap_accounts SET host = $3, port = $4, security = $5, username = $6, password = $7, folder = $8,
			uid_validity = $9, last_uid = $10, last_error = '', checked_at = now() WHERE source_id = $1`,
		old.SourceID, name, a.Host, a.Port, a.Security, a.Username, sealed, a.Folder, int64(validity), int64(newest)); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}

// checkMailboxHandler checks a mailbox right away ("Check now").
func (s *Server) checkMailboxHandler(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ownMailbox(w, r)
	if !ok {
		return
	}
	if err := s.checkMailboxNow(r.Context(), a.SourceID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, "/tasks/sources")
}
