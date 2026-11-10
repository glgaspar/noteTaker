package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Microsoft 365 and Outlook.com don't accept passwords over IMAP, only OAuth tokens. The
// user signs in with Microsoft's device code flow (a code typed at microsoft.com/devicelogin,
// so no redirect URL or client secret is needed); the refresh token is kept encrypted and
// exchanged for an access token on each check, which logs in with SASL XOAUTH2.
//
// The server needs an app registration in Microsoft Entra: public client flows allowed, and
// the delegated permissions IMAP.AccessAsUser.All and offline_access (see the README).

// MicrosoftConfig is the app registration; empty ClientID turns Microsoft mailboxes off.
type MicrosoftConfig struct {
	ClientID string
	Tenant   string // "common" (work and personal accounts), "organizations", or a tenant ID
}

const msScope = "https://outlook.office.com/IMAP.AccessAsUser.All offline_access openid email profile"

// Where Microsoft is; tests point these at fakes.
var (
	msLoginURL = "https://login.microsoftonline.com"
	msIMAPHost = "outlook.office365.com"
	msIMAPPort = 993
)

var msClient = &http.Client{Timeout: 30 * time.Second}

// xoauth2 is the SASL XOAUTH2 mechanism Microsoft (and Google) use for IMAP.
type xoauth2 struct{ user, token string }

func (x xoauth2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + x.user + "\x01auth=Bearer " + x.token + "\x01\x01"), nil
}

// Next answers the server's only challenge, a JSON error, with nothing, so it reports the failure.
func (x xoauth2) Next([]byte) ([]byte, error) { return []byte{}, nil }

// msError is Microsoft's OAuth error answer.
type msError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *msError) Error() string {
	desc, _, _ := strings.Cut(e.Description, "Trace ID:") // drop the trace and correlation ids
	return fmt.Sprintf("Microsoft said %s: %s", e.Code, strings.TrimSpace(desc))
}

// msPost posts a form to the tenant's OAuth endpoint ("devicecode" or "token").
func (s *Server) msPost(ctx context.Context, endpoint string, form url.Values, out any) error {
	form.Set("client_id", s.microsoft.ClientID)
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/%s/oauth2/v2.0/%s", msLoginURL, url.PathEscape(s.microsoft.Tenant), endpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := msClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e msError
		if json.Unmarshal(body, &e) != nil || e.Code == "" {
			return fmt.Errorf("Microsoft answered %d: %.200s", res.StatusCode, body)
		}
		return &e
	}
	return json.Unmarshal(body, out)
}

type msTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

// msAccessToken exchanges a mailbox's refresh token (a.Password) for an access token, and
// stores the new refresh token Microsoft hands out.
func (s *Server) msAccessToken(ctx context.Context, a *mailAccount) error {
	if s.microsoft.ClientID == "" {
		return errors.New("Microsoft sign-in is not set up on this server (MS_CLIENT_ID)")
	}
	var t msTokens
	err := s.msPost(ctx, "token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.Password}, "scope": {msScope}}, &t)
	var me *msError
	if errors.As(err, &me) && me.Code == "invalid_grant" {
		return fmt.Errorf("the Microsoft sign-in expired or was revoked; use Reconnect (%v)", err)
	}
	if err != nil {
		return err
	}
	if t.RefreshToken != "" && t.RefreshToken != a.Password {
		if _, err := s.db.Exec(ctx, `UPDATE imap_accounts SET password = $2 WHERE source_id = $1`,
			a.SourceID, s.secrets.seal(t.RefreshToken)); err != nil {
			return err
		}
	}
	a.Token = t.AccessToken
	return nil
}

// idTokenEmail reads the account's address from an ID token. Its signature isn't checked:
// it came straight from Microsoft over TLS, and only names the mailbox to open.
func idTokenEmail(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	json.Unmarshal(payload, &claims)
	if strings.Contains(claims.PreferredUsername, "@") {
		return claims.PreferredUsername
	}
	return claims.Email
}

// ---- signing in ----

// msSignIn is a device code sign-in waiting for the user, kept in memory (it lasts 15 minutes).
type msSignIn struct {
	mu         sync.Mutex // one poll at a time
	UserID     int64
	SourceID   int64 // set when reconnecting a mailbox
	Name       string
	Folder     string
	Tags       []string
	DeviceCode string
	UserCode   string
	LoginURL   string
	Expires    time.Time
	Interval   time.Duration
	NextPoll   time.Time
	Done       bool
	Error      string
}

type msSignIns struct {
	sync.Mutex
	m map[string]*msSignIn
}

// startMicrosoft begins a sign-in for a new mailbox, or a mailbox to reconnect ("source_id").
func (s *Server) startMicrosoft(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	switch {
	case s.microsoft.ClientID == "":
		s.mailboxError(w, r, 0, "Microsoft sign-in is not set up on this server (MS_CLIENT_ID).")
		return
	case s.secrets == nil:
		s.mailboxError(w, r, 0, "Mailboxes can't be added: the server has no SECRET_KEY set.")
		return
	}
	in := &msSignIn{UserID: user.ID, Name: strings.TrimSpace(r.FormValue("name")), Folder: strings.TrimSpace(r.FormValue("folder")), Tags: formTags(r)}
	if id := formID(r, "source_id"); id != nil {
		var auth string
		err := s.db.QueryRow(ctx, `SELECT a.auth FROM imap_accounts a JOIN task_sources s ON s.id = a.source_id
			WHERE a.source_id = $1 AND s.user_id = $2`, *id, user.ID).Scan(&auth)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && auth != "microsoft") {
			s.fail(w, r, http.StatusNotFound, "")
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		in.SourceID = *id
	}
	if in.Folder == "" {
		in.Folder = "INBOX"
	}
	var dev struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := s.msPost(ctx, "devicecode", url.Values{"scope": {msScope}}, &dev); err != nil {
		s.mailboxError(w, r, in.SourceID, "Couldn't start the Microsoft sign-in: "+err.Error())
		return
	}
	in.DeviceCode, in.UserCode, in.LoginURL = dev.DeviceCode, dev.UserCode, dev.VerificationURI
	in.Expires = time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	in.Interval = time.Duration(dev.Interval) * time.Second
	id := rand.Text()
	s.msPending.Lock()
	for k, p := range s.msPending.m { // forget abandoned sign-ins
		if time.Now().After(p.Expires) {
			delete(s.msPending.m, k)
		}
	}
	s.msPending.m[id] = in
	s.msPending.Unlock()
	redirect(w, r, "/tasks/microsoft/"+id)
}

// microsoftPage shows the code to enter; with HX-Request it is the page's poll, which asks
// Microsoft whether the user finished, and saves the mailbox when they did.
func (s *Server) microsoftPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.msPending.Lock()
	in := s.msPending.m[id]
	s.msPending.Unlock()
	if in == nil || in.UserID != currentUser(r).ID {
		s.fail(w, r, http.StatusNotFound, "this sign-in has ended; start again from the sources page")
		return
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		if err := s.pollMicrosoft(r.Context(), id, in); err != nil {
			s.serverError(w, r, err)
			return
		}
		in.mu.Lock()
		defer in.mu.Unlock()
		if in.Done {
			redirect(w, r, "/tasks/sources")
			return
		}
		s.renderPartial(w, "microsoft.html", "ms_status", map[string]any{"ID": id, "SignIn": in})
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	s.render(w, "microsoft.html", map[string]any{"User": currentUser(r), "ID": id, "SignIn": in})
}

// pollMicrosoft asks for the tokens once (no faster than Microsoft allows). On success it
// tries the mailbox and saves it; the user's problems end up in in.Error, database errors
// are returned.
func (s *Server) pollMicrosoft(ctx context.Context, id string, in *msSignIn) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch {
	case in.Done || in.Error != "" || time.Now().Before(in.NextPoll):
		return nil
	case time.Now().After(in.Expires):
		in.Error = "The code expired before the sign-in finished. Start again."
		return nil
	}
	in.NextPoll = time.Now().Add(in.Interval)
	var t msTokens
	err := s.msPost(ctx, "token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {in.DeviceCode}}, &t)
	var me *msError
	if errors.As(err, &me) && me.Code == "authorization_pending" {
		return nil
	}
	if errors.As(err, &me) && me.Code == "slow_down" {
		in.Interval += 5 * time.Second
		return nil
	}
	if err != nil {
		in.Error = err.Error()
		return nil
	}
	email := idTokenEmail(t.IDToken)
	if email == "" || t.RefreshToken == "" {
		in.Error = "Microsoft didn't say which mailbox this is, or gave no lasting sign-in (is offline_access allowed?)."
		return nil
	}
	a := mailAccount{SourceID: in.SourceID, UserID: in.UserID, Host: msIMAPHost, Port: msIMAPPort, Security: "tls",
		Username: email, Folder: in.Folder, Auth: "microsoft", Token: t.AccessToken}
	validity, newest, err := tryMailbox(a)
	if err != nil {
		in.Error = fmt.Sprintf("Signed in as %s, but the mailbox couldn't be opened (is IMAP enabled for it?): %v", email, err)
		return nil
	}
	if err := s.saveMicrosoftMailbox(ctx, in, a, s.secrets.seal(t.RefreshToken), validity, newest); err != nil {
		return err
	}
	in.Done = true
	s.msPending.Lock()
	delete(s.msPending.m, id)
	s.msPending.Unlock()
	return nil
}

func (s *Server) saveMicrosoftMailbox(ctx context.Context, in *msSignIn, a mailAccount, sealed []byte, validity, newest uint32) error {
	if in.SourceID != 0 {
		// Reconnecting the same mailbox carries on where it was; another one starts from now.
		old, err := s.loadMailAccount(ctx, in.SourceID)
		if err != nil {
			return err
		}
		if old.Username == a.Username && old.Folder == a.Folder && old.UIDValidity == validity {
			newest = old.LastUID
		}
		_, err = s.db.Exec(ctx, `UPDATE imap_accounts SET username = $2, password = $3, folder = $4, uid_validity = $5,
			last_uid = $6, last_error = '', checked_at = now() WHERE source_id = $1`,
			in.SourceID, a.Username, sealed, a.Folder, int64(validity), int64(newest))
		return err
	}
	name := in.Name
	if name == "" {
		name = a.Username
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO task_sources (user_id, kind, name, tags) VALUES ($1, 'imap', $2, $3) RETURNING id`,
		in.UserID, name, in.Tags).Scan(&id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO imap_accounts (source_id, host, port, security, username, password, folder,
		uid_validity, last_uid, checked_at, auth) VALUES ($1, $2, $3, 'tls', $4, $5, $6, $7, $8, now(), 'microsoft')`,
		id, a.Host, a.Port, a.Username, sealed, a.Folder, int64(validity), int64(newest)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
