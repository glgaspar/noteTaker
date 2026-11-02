package app

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestLoginAndSessions(t *testing.T) {
	a := newApp(t, false)
	u := a.user("Ann", false)
	anon := a.client()

	if res := anon.get("/"); res.location != "/login" {
		t.Errorf("anonymous / redirects to %q, want /login", res.location)
	}
	if res := anon.post("/login", url.Values{"email": {u.Email}, "password": {"wrong"}}); !strings.Contains(res.body, "Invalid email or password") {
		t.Errorf("bad password: %d", res.code)
	}
	if res := u.get("/"); res.code != http.StatusOK {
		t.Errorf("logged in / = %d", res.code)
	}
	u.post("/logout", nil)
	if res := u.get("/"); res.location != "/login" {
		t.Errorf("after logout / redirects to %q", res.location)
	}
}

func TestLoginRateLimit(t *testing.T) {
	a := newApp(t, false)
	u := a.user("Ann", false)
	other := a.user("Ben", false)
	c := a.client()
	for range 5 {
		c.post("/login", url.Values{"email": {u.Email}, "password": {"wrong"}})
	}
	if res := c.post("/login", url.Values{"email": {u.Email}, "password": {"password"}}); !strings.Contains(res.body, "Too many failed attempts") {
		t.Errorf("6th attempt with the right password should be blocked, got %d", res.code)
	}
	if res := c.post("/login", url.Values{"email": {other.Email}, "password": {"password"}}); res.code != http.StatusSeeOther {
		t.Errorf("another account from the same address should still log in, got %d", res.code)
	}
}

var linkRe = regexp.MustCompile(`value="(http[^"]+/(?:invite|reset)/[A-Z0-9]+)"`)

// linkFrom extracts the link the admin page shows when SMTP is not configured.
func linkFrom(t *testing.T, res response) string {
	t.Helper()
	m := linkRe.FindStringSubmatch(res.body)
	if m == nil {
		t.Fatalf("no link on page (status %d): %.300s", res.code, res.body)
	}
	return m[1]
}

func TestInviteFlow(t *testing.T) {
	a := newApp(t, false)
	admin := a.user("Admin", true)
	plain := a.user("Plain", false)

	if res := plain.post("/admin/invites", url.Values{"email": {"x@example.com"}}); res.code != http.StatusForbidden {
		t.Errorf("non-admin invite = %d", res.code)
	}
	email := fmt.Sprintf("new-%d@example.com", seq.Add(1))
	link := linkFrom(t, admin.post("/admin/invites", url.Values{"email": {email}}))
	path := strings.TrimPrefix(link, a.url)

	guest := a.client()
	if res := guest.post(path, url.Values{"name": {"New"}, "password": {"short"}}); !strings.Contains(res.body, "at least 8") {
		t.Errorf("short password accepted: %d", res.code)
	}
	if res := guest.post(path, url.Values{"name": {"New"}, "password": {"longenough"}}); res.code != http.StatusSeeOther {
		t.Fatalf("accept invite = %d", res.code)
	}
	if res := guest.get("/"); res.code != http.StatusOK {
		t.Errorf("new user not logged in: %d", res.code)
	}
	if res := a.client().get(path); !strings.Contains(res.body, "Invite not found") {
		t.Error("invite link works twice")
	}
}

func TestPasswordReset(t *testing.T) {
	a := newApp(t, false)
	admin := a.user("Admin", true)
	u := a.user("Ann", false)

	link := linkFrom(t, admin.post(fmt.Sprintf("/admin/users/%d/reset", u.ID), nil))
	path := strings.TrimPrefix(link, a.url)
	fresh := a.client()
	if res := fresh.post(path, url.Values{"password": {"brandnewpass"}}); res.code != http.StatusSeeOther {
		t.Fatalf("reset = %d %.200s", res.code, res.body)
	}
	if res := u.get("/"); res.location != "/login" {
		t.Error("old session survived a password reset")
	}
	if res := a.client().get(path); !strings.Contains(res.body, "Link expired") {
		t.Error("reset link works twice")
	}
	if res := a.client().post("/login", url.Values{"email": {u.Email}, "password": {"brandnewpass"}}); res.code != http.StatusSeeOther {
		t.Errorf("login with new password = %d", res.code)
	}

	// Self-service never reveals whether the account exists, nor the link.
	for _, email := range []string{u.Email, "nobody@example.com"} {
		res := a.client().post("/forgot", url.Values{"email": {email}})
		if !strings.Contains(res.body, "If an account exists") || strings.Contains(res.body, "/reset/") {
			t.Errorf("forgot %s: unexpected page", email)
		}
	}
}

func TestDisableUser(t *testing.T) {
	a := newApp(t, false)
	admin := a.user("Admin", true)
	u := a.user("Ann", false)
	ws := admin.workspace("W")

	if res := admin.post(fmt.Sprintf("/admin/users/%d/disable", admin.ID), nil); !strings.Contains(res.body, "cannot disable yourself") {
		t.Error("admin could disable themselves")
	}
	admin.post(fmt.Sprintf("/admin/users/%d/disable", u.ID), nil)
	if res := u.get("/"); res.location != "/login" {
		t.Error("disabled user's session still works")
	}
	if res := a.client().post("/login", url.Values{"email": {u.Email}, "password": {"password"}}); res.code == http.StatusSeeOther {
		t.Error("disabled user can log in")
	}
	if res := admin.post(fmt.Sprintf("/w/%d/members", ws), url.Values{"email": {u.Email}, "role": {"editor"}}); !strings.Contains(res.body, "No user with that email") {
		t.Error("disabled user can be added to a workspace")
	}
	admin.post(fmt.Sprintf("/admin/users/%d/enable", u.ID), nil)
	if res := a.client().post("/login", url.Values{"email": {u.Email}, "password": {"password"}}); res.code != http.StatusSeeOther {
		t.Error("re-enabled user cannot log in")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 60e9)
	l.fail("k")
	if blocked, _ := l.blocked("k"); blocked {
		t.Fatal("blocked after 1 of 2")
	}
	l.fail("k")
	if blocked, _ := l.blocked("k"); !blocked {
		t.Fatal("not blocked after 2 of 2")
	}
	l.clear("k")
	if blocked, _ := l.blocked("k"); blocked {
		t.Fatal("still blocked after clear")
	}
}

func TestSecurityHeaders(t *testing.T) {
	a := newApp(t, false)
	res := a.client().get("/login")
	for header, want := range map[string]string{
		"Content-Security-Policy": "script-src 'self';",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	} {
		if got := res.header.Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", header, got, want)
		}
	}
	if strings.Contains(res.body, "onclick=") || strings.Contains(res.body, "<script>") {
		t.Error("page has inline script, which the CSP blocks")
	}
}
