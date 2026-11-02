package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPermissions checks every role against every kind of action. Guests have a share on the
// "Shared" folder only, which contains the "Sub" folder.
func TestPermissions(t *testing.T) {
	a := newApp(t, false)
	owner := a.user("Owner", false)
	ws := owner.workspace("Team")
	shared := owner.folder(ws, "Shared", 0)
	sub := owner.folder(ws, "Sub", shared)
	rootNote := owner.note(ws, 0, "RootNote", "alphaword")
	subNote := owner.note(ws, sub, "SubNote", "betaword")

	editor, viewer := a.user("Editor", false), a.user("Viewer", false)
	guestEditor, guestViewer := a.user("GuestEditor", false), a.user("GuestViewer", false)
	outsider := a.user("Outsider", false)
	owner.share(fmt.Sprintf("/w/%d/members", ws), editor.Email, "editor")
	owner.share(fmt.Sprintf("/w/%d/members", ws), viewer.Email, "viewer")
	owner.share(fmt.Sprintf("/folders/%d/share", shared), guestEditor.Email, "editor")
	owner.share(fmt.Sprintf("/folders/%d/share", shared), guestViewer.Email, "viewer")

	users := []*user{owner, editor, viewer, guestEditor, guestViewer, outsider}
	names := []string{"owner", "editor", "viewer", "guest editor", "guest viewer", "outsider"}
	saveNote := func(u *user, id int64, title, text string) bool {
		return u.putJSON(fmt.Sprintf("/notes/%d", id), map[string]any{"title": title, "content": nil, "text": text}).code == http.StatusNoContent
	}
	shows := func(u *user, path, text string) bool {
		res := u.get(path)
		return res.code == http.StatusOK && strings.Contains(res.body, text)
	}
	cases := []struct {
		action string
		do     func(u *user) bool
		want   [6]bool // owner, editor, viewer, guest editor, guest viewer, outsider
	}{
		{"open workspace", func(u *user) bool { return u.get(fmt.Sprintf("/w/%d", ws)).code == http.StatusOK },
			[6]bool{true, true, true, true, true, false}},
		{"open root note", func(u *user) bool { return shows(u, fmt.Sprintf("/w/%d?note=%d", ws, rootNote), `id="editor"`) },
			[6]bool{true, true, true, false, false, false}},
		{"open shared note", func(u *user) bool { return shows(u, fmt.Sprintf("/w/%d?note=%d", ws, subNote), `id="editor"`) },
			[6]bool{true, true, true, true, true, false}},
		{"edit shared note", func(u *user) bool { return saveNote(u, subNote, "SubNote", "betaword") },
			[6]bool{true, true, false, true, false, false}},
		{"edit root note", func(u *user) bool { return saveNote(u, rootNote, "RootNote", "alphaword") },
			[6]bool{true, true, false, false, false, false}},
		{"connect to shared note", func(u *user) bool { return u.get(fmt.Sprintf("/notes/%d/ws", subNote)).code != http.StatusNotFound },
			[6]bool{true, true, true, true, true, false}},
		{"create note in shared folder", func(u *user) bool {
			return u.post(fmt.Sprintf("/w/%d/notes", ws), url.Values{"folder_id": {fmt.Sprint(shared)}}).code == http.StatusSeeOther
		}, [6]bool{true, true, false, true, false, false}},
		{"create root folder", func(u *user) bool {
			return u.post(fmt.Sprintf("/w/%d/folders", ws), url.Values{"name": {"X"}}).code == http.StatusSeeOther
		}, [6]bool{true, true, false, false, false, false}},
		{"rename shared folder", func(u *user) bool {
			return u.post(fmt.Sprintf("/folders/%d/rename", shared), url.Values{"name": {"Shared"}}).code == http.StatusSeeOther
		}, [6]bool{true, true, false, false, false, false}},
		{"rename subfolder", func(u *user) bool {
			return u.post(fmt.Sprintf("/folders/%d/rename", sub), url.Values{"name": {"Sub"}}).code == http.StatusSeeOther
		}, [6]bool{true, true, false, true, false, false}},
		{"open folder sharing", func(u *user) bool { return u.get(fmt.Sprintf("/folders/%d/share", shared)).code == http.StatusOK },
			[6]bool{true, true, false, false, false, false}},
		{"open members", func(u *user) bool { return u.get(fmt.Sprintf("/w/%d/members", ws)).code == http.StatusOK },
			[6]bool{true, true, true, false, false, false}},
		{"add member", func(u *user) bool {
			target := a.user("Target", false)
			return u.post(fmt.Sprintf("/w/%d/members", ws), url.Values{"email": {target.Email}, "role": {"viewer"}}).code == http.StatusSeeOther
		}, [6]bool{true, false, false, false, false, false}},
		{"find root note", func(u *user) bool { return shows(u, fmt.Sprintf("/w/%d/search?q=alphaword", ws), "RootNote") },
			[6]bool{true, true, true, false, false, false}},
		{"find shared note", func(u *user) bool { return shows(u, fmt.Sprintf("/w/%d/search?q=betaword", ws), "SubNote") },
			[6]bool{true, true, true, true, true, false}},
	}
	for _, c := range cases {
		for i, u := range users {
			if got := c.do(u); got != c.want[i] {
				t.Errorf("%s: %s = %v, want %v", names[i], c.action, got, c.want[i])
			}
		}
	}
}

func TestRevokedShare(t *testing.T) {
	a := newApp(t, false)
	owner := a.user("Owner", false)
	guest := a.user("Guest", false)
	ws := owner.workspace("W")
	f := owner.folder(ws, "F", 0)
	n := owner.note(ws, f, "N", "text")
	owner.share(fmt.Sprintf("/folders/%d/share", f), guest.Email, "editor")
	if res := guest.get(fmt.Sprintf("/w/%d", ws)); res.code != http.StatusOK {
		t.Fatalf("guest cannot open workspace: %d", res.code)
	}
	owner.post(fmt.Sprintf("/folders/%d/share/%d/delete", f, guest.ID), nil)
	if res := guest.putJSON(fmt.Sprintf("/notes/%d", n), map[string]any{"title": "x"}); res.code != http.StatusNotFound {
		t.Errorf("revoked guest edit = %d", res.code)
	}
	if res := guest.get(fmt.Sprintf("/w/%d", ws)); res.code != http.StatusNotFound {
		t.Errorf("revoked guest still sees workspace: %d", res.code)
	}
}

func TestFolderMove(t *testing.T) {
	a := newApp(t, false)
	owner := a.user("Owner", false)
	guest := a.user("Guest", false)
	ws := owner.workspace("W")
	parent := owner.folder(ws, "Parent", 0)
	child := owner.folder(ws, "Child", parent)
	other := owner.folder(ws, "Other", 0)
	elsewhere := owner.folder(owner.workspace("Elsewhere"), "Far", 0)
	move := func(u *user, folder, to int64) int {
		return u.post(fmt.Sprintf("/folders/%d/move", folder), url.Values{"parent_id": {fmt.Sprint(to)}}).code
	}

	if code := move(owner, parent, child); code != http.StatusBadRequest {
		t.Errorf("move into own child = %d", code)
	}
	if code := move(owner, parent, parent); code != http.StatusBadRequest {
		t.Errorf("move into itself = %d", code)
	}
	if code := move(owner, parent, elsewhere); code != http.StatusBadRequest {
		t.Errorf("move to another workspace = %d", code)
	}
	if code := move(owner, parent, other); code != http.StatusSeeOther {
		t.Errorf("valid move = %d", code)
	}
	if got := dbInt(t, `SELECT parent_id FROM folders WHERE id = $1`, parent); got != other {
		t.Errorf("parent is %d, want %d", got, other)
	}
	if res := owner.post(fmt.Sprintf("/folders/%d/move", parent), url.Values{"parent_id": {""}}); res.code != http.StatusSeeOther {
		t.Errorf("move to root = %d", res.code)
	}
	// A guest editor of a folder may reorganize inside it, but not move the shared folder itself.
	owner.share(fmt.Sprintf("/folders/%d/share", parent), guest.Email, "editor")
	if code := move(guest, parent, other); code == http.StatusSeeOther {
		t.Error("guest moved the shared folder")
	}
	sibling := owner.folder(ws, "Sibling", parent)
	if code := move(guest, child, sibling); code != http.StatusSeeOther {
		t.Errorf("guest move inside share = %d", code)
	}
}
