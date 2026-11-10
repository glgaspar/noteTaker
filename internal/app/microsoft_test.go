package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeMicrosoft is the tenant's OAuth endpoints. A device code is approved on its second poll
// (the first answers authorization_pending), unless declined is set. Refresh tokens rotate.
type fakeMicrosoft struct {
	*httptest.Server
	mu       sync.Mutex
	polls    int
	declined bool
	access   string   // the access token handed out (the test IMAP server's password)
	refresh  []string // refresh tokens received, in order
}

func newFakeMicrosoft(t *testing.T, email string) *fakeMicrosoft {
	f := &fakeMicrosoft{access: "right"}
	idToken := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"preferred_username":"`+email+`"}`)) + ".y"
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		fail := func(code string) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "AADSTS0: " + code + "\r\nTrace ID: 1"})
		}
		if !strings.HasPrefix(r.URL.Path, "/contoso/oauth2/v2.0/") || r.FormValue("client_id") != "cid" {
			fail("invalid_client")
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/devicecode"):
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD-1234",
				"verification_uri": "https://microsoft.com/devicelogin", "expires_in": 900, "interval": 0})
		case r.FormValue("grant_type") == "urn:ietf:params:oauth:grant-type:device_code":
			if f.polls++; f.declined {
				fail("authorization_declined")
			} else if f.polls%2 == 1 {
				fail("authorization_pending")
			} else {
				json.NewEncoder(w).Encode(map[string]string{"access_token": f.access, "refresh_token": "r1", "id_token": idToken})
			}
		case r.FormValue("grant_type") == "refresh_token":
			f.refresh = append(f.refresh, r.FormValue("refresh_token"))
			if r.FormValue("refresh_token") == "revoked" {
				fail("invalid_grant")
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": f.access, "refresh_token": "r" + strconv.Itoa(len(f.refresh)+1)})
		default:
			fail("unsupported_grant_type")
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// poll is the sign-in page's own poll (an htmx request); it returns the HX-Redirect, if any.
func (u *user) poll(path string) (response, string) {
	u.t.Helper()
	req, _ := http.NewRequest("GET", u.base+path, nil)
	req.Header.Set("HX-Request", "true")
	res := u.do(req)
	return res, res.header.Get("HX-Redirect")
}

func TestMicrosoftMailbox(t *testing.T) {
	a := newApp(t, false)
	a.s.secrets = newSealer("test secret")
	ctx := context.Background()
	mail := newMailServer(t)
	mail.addUser(t, "ann@contoso.com", "right")
	ms := newFakeMicrosoft(t, "ann@contoso.com")
	oldLogin, oldHost, oldPort := msLoginURL, msIMAPHost, msIMAPPort
	msLoginURL, msIMAPHost = ms.URL, "127.0.0.1"
	msIMAPPort, _ = strconv.Atoi(mail.port)
	t.Cleanup(func() { msLoginURL, msIMAPHost, msIMAPPort = oldLogin, oldHost, oldPort })
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	t.Cleanup(func() { testDB.Exec(ctx, `DELETE FROM task_sources WHERE user_id = $1`, ann.ID) })

	if res := ann.post("/tasks/microsoft", nil); !strings.Contains(res.body, "MS_CLIENT_ID") {
		t.Error("without MS_CLIENT_ID the page should say Microsoft isn't set up")
	}
	a.s.microsoft = MicrosoftConfig{ClientID: "cid", Tenant: "contoso"}

	// sign in: the page shows the code, and its poll saves the mailbox once Microsoft approves
	res := ann.post("/tasks/microsoft", url.Values{"tags": {"work"}})
	signIn := res.location
	if !strings.HasPrefix(signIn, "/tasks/microsoft/") {
		t.Fatalf("starting the sign-in: %d %q %.300s", res.code, signIn, res.body)
	}
	if body := ann.get(signIn).body; !strings.Contains(body, "ABCD-1234") || !strings.Contains(body, "https://microsoft.com/devicelogin") {
		t.Error("the page should show the code and where to enter it")
	}
	if res := ben.get(signIn); res.code != http.StatusNotFound {
		t.Errorf("Ben opening Ann's sign-in: %d", res.code)
	}
	if _, to := ann.poll(signIn); to != "" {
		t.Errorf("the first poll (pending) redirected to %q", to)
	}
	if _, to := ann.poll(signIn); to != "/tasks/sources" {
		t.Fatalf("after approval the poll should go to the sources, got %q", to)
	}
	source := dbInt(t, `SELECT s.id FROM task_sources s JOIN imap_accounts a ON a.source_id = s.id WHERE s.user_id = $1
		AND s.name = 'ann@contoso.com' AND s.tags = '{work}' AND a.auth = 'microsoft' AND a.username = 'ann@contoso.com'`, ann.ID)
	stored := func() string {
		acc, err := a.s.loadMailAccount(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		token, err := a.s.secrets.open([]byte(acc.Password))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	if stored() != "r1" {
		t.Error("the refresh token should be stored (encrypted)")
	}
	if res := ann.get("/tasks/sources"); !strings.Contains(res.body, "ann@contoso.com · Microsoft") || !strings.Contains(res.body, "Reconnect") {
		t.Error("the sources page should list the Microsoft mailbox with Reconnect")
	}

	// checks log in with a fresh access token, and keep the new refresh token
	mail.deliver(t, "ann@contoso.com", "INBOX", message("ms1", "Approve the invoice", "text/plain", "From finance."))
	a.s.checkMailboxNow(ctx, source)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE source_id = $1 AND title = 'Approve the invoice' AND tags = '{work}'`, source); n != 1 {
		t.Errorf("new mail from the Microsoft mailbox: %d items", n)
	}
	if stored() != "r2" || fmt.Sprint(ms.refresh) != "[r1]" {
		t.Errorf("refresh token rotation: stored %q, sent %v", stored(), ms.refresh)
	}

	// a revoked sign-in asks for Reconnect, which mends it in place
	testDB.Exec(ctx, `UPDATE imap_accounts SET password = $2 WHERE source_id = $1`, source, a.s.secrets.seal("revoked"))
	a.s.checkMailboxNow(ctx, source)
	if n := dbInt(t, `SELECT count(*) FROM imap_accounts WHERE source_id = $1 AND last_error LIKE '%Reconnect%'`, source); n != 1 {
		t.Error("a revoked sign-in should ask for Reconnect")
	}
	if res := ben.post("/tasks/microsoft", url.Values{"source_id": {fmt.Sprint(source)}}); res.code != http.StatusNotFound {
		t.Errorf("Ben reconnecting Ann's mailbox: %d", res.code)
	}
	reconnect := ann.post("/tasks/microsoft", url.Values{"source_id": {fmt.Sprint(source)}}).location
	ann.poll(reconnect)
	if _, to := ann.poll(reconnect); to != "/tasks/sources" || stored() != "r1" {
		t.Errorf("reconnect: redirect %q, stored token %q", to, stored())
	}
	if n := dbInt(t, `SELECT count(*) FROM task_sources WHERE user_id = $1`, ann.ID); n != 1 {
		t.Errorf("reconnecting made %d sources, want 1", n)
	}
	a.s.checkMailboxNow(ctx, source)
	if n := dbInt(t, `SELECT count(*) FROM inbox_items WHERE source_id = $1`, source); n != 1 {
		t.Errorf("reconnecting re-imported mail: %d items", n)
	}
	if res := ann.post(fmt.Sprintf("/tasks/mailboxes/%d/edit", source), url.Values{"host": {"x"}, "username": {"y"}}); !strings.Contains(res.body, "Reconnect") {
		t.Error("editing a Microsoft mailbox should point to Reconnect")
	}

	// a declined sign-in, and one whose mailbox won't open, end with the reason
	ms.declined = true
	declined := ann.post("/tasks/microsoft", nil).location
	if res, _ := ann.poll(declined); !strings.Contains(res.body, "authorization_declined") || strings.Contains(res.body, "Trace ID") {
		t.Errorf("a declined sign-in should show Microsoft's reason (without its trace): %.300s", res.body)
	}
	ms.declined, ms.access = false, "wrong"
	noIMAP := ann.post("/tasks/microsoft", nil).location
	ann.poll(noIMAP)
	if res, _ := ann.poll(noIMAP); !strings.Contains(res.body, "is IMAP enabled") {
		t.Errorf("a mailbox that won't open should say so: %.300s", res.body)
	}
	if n := dbInt(t, `SELECT count(*) FROM task_sources WHERE user_id = $1`, ann.ID); n != 1 {
		t.Errorf("failed sign-ins added sources: %d", n)
	}
}
