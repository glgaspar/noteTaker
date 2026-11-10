package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (u *user) task(title string, form url.Values) int64 {
	u.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("title", title)
	res := u.post("/tasks", form)
	if res.code != http.StatusSeeOther {
		u.t.Fatalf("create task %q: %d %.200s", title, res.code, res.body)
	}
	return createdID(u.t, res)
}

// taskTitles lists the task titles shown in a /tasks list.
func (u *user) taskTitles(query string) []string {
	u.t.Helper()
	res := u.get("/tasks" + query)
	if res.code != http.StatusOK {
		u.t.Fatalf("tasks%s: %d", query, res.code)
	}
	var titles []string
	for _, part := range strings.Split(res.body, "data-task-title>")[1:] {
		titles = append(titles, part[:strings.Index(part, "<")])
	}
	return titles
}

func TestTasks(t *testing.T) {
	a := newApp(t, false)
	ann := a.user("Ann", false)
	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)

	later := ann.task("Later", nil)
	soon := ann.task("Soon", url.Values{"due_on": {tomorrow}})
	id := func(n int64) string { return strconv.FormatInt(n, 10) }

	// subtasks show on the parent, not in the main list, and nest one level only
	sub := ann.task("Step one", url.Values{"parent_id": {id(soon)}})
	if sub != soon {
		t.Errorf("adding a subtask should return to the parent %d, got %d", soon, sub)
	}
	subID := dbInt(t, `SELECT id FROM tasks WHERE parent_id = $1`, soon)
	if res := ann.post("/tasks", url.Values{"title": {"Too deep"}, "parent_id": {id(subID)}}); res.code != http.StatusNotFound {
		t.Errorf("sub-subtask: %d, want 404", res.code)
	}
	if got := fmt.Sprint(ann.taskTitles("")); got != "[Soon Later]" {
		t.Errorf("list (due first, no subtasks): %s", got)
	}
	if body := ann.get("/tasks?task=" + id(soon)).body; !strings.Contains(body, "Step one") || !strings.Contains(body, "Tomorrow") {
		t.Error("task page should show its subtask and due date")
	}

	// editing, and an empty title keeps the old one
	ann.post("/tasks/"+id(later), url.Values{"title": {""}, "details": {"some notes"}, "due_on": {""}})
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE id = $1 AND title = 'Later' AND details = 'some notes' AND due_on IS NULL`, later); n != 1 {
		t.Error("update did not keep the title or save the details")
	}

	// tags: a tagged subtask shows up under the tag, and new tasks there get the tag
	ann.post("/tasks/"+id(subID)+"/tags", url.Values{"name": {"#home"}})
	tag := dbInt(t, `SELECT id FROM task_tags WHERE name = 'home' AND user_id = $1`, ann.ID)
	ann.task("Groceries", url.Values{"tag": {id(tag)}})
	if got := fmt.Sprint(ann.taskTitles("?tag=" + id(tag))); got != "[Step one Groceries]" {
		t.Errorf("tag filter: %s", got)
	}
	ann.post(fmt.Sprintf("/tasks/%d/tags/%d/delete", subID, tag), nil)
	if got := fmt.Sprint(ann.taskTitles("?tag=" + id(tag))); got != "[Groceries]" {
		t.Errorf("after removing the tag: %s", got)
	}

	// deleting a task deletes its subtasks
	ann.post("/tasks/"+id(soon)+"/delete", nil)
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE id IN ($1, $2)`, soon, subID); n != 0 {
		t.Errorf("%d tasks left after delete", n)
	}
}

func TestTasksArePrivate(t *testing.T) {
	a := newApp(t, false)
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	secret := ann.task("Ann's secret", nil)
	ann.post(fmt.Sprintf("/tasks/%d/tags", secret), url.Values{"name": {"private"}})
	tag := dbInt(t, `SELECT id FROM task_tags WHERE user_id = $1`, ann.ID)
	path := fmt.Sprintf("/tasks/%d", secret)

	if res := ben.get("/tasks?task=" + strconv.FormatInt(secret, 10)); res.code != http.StatusNotFound {
		t.Errorf("Ben opening Ann's task: %d", res.code)
	}
	for _, p := range []string{path, path + "/delete", path + "/done", path + "/tags", fmt.Sprintf("%s/tags/%d/delete", path, tag)} {
		if res := ben.post(p, url.Values{"title": {"hacked"}, "name": {"x"}}); res.code != http.StatusNotFound {
			t.Errorf("Ben POST %s: %d", p, res.code)
		}
	}
	if res := ben.post("/tasks", url.Values{"title": {"x"}, "parent_id": {strconv.FormatInt(secret, 10)}}); res.code != http.StatusNotFound {
		t.Errorf("Ben adding a subtask to Ann's task: %d", res.code)
	}
	// Ben's own tag filter cannot pull in Ann's tag
	ben.task("Ben task", url.Values{"tag": {strconv.FormatInt(tag, 10)}})
	if got := ben.taskTitles("?tag=" + strconv.FormatInt(tag, 10)); len(got) != 0 {
		t.Errorf("Ben sees through Ann's tag: %v", got)
	}
	if got := fmt.Sprint(ben.taskTitles("")); got != "[Ben task]" {
		t.Errorf("Ben's list: %s", got)
	}
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE id = $1 AND title = 'Ann''s secret'`, secret); n != 1 {
		t.Error("Ann's task changed")
	}
}

func TestTodayAndCompleted(t *testing.T) {
	a := newApp(t, false)
	ann, ben := a.user("Ann", false), a.user("Ben", false)
	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	report, bills, gym := ann.task("Report", nil), ann.task("Bills", nil), ann.task("Gym", nil)
	ann.task("Numbers", url.Values{"parent_id": {id(report)}})
	ann.task("Summary", url.Values{"parent_id": {id(report)}})
	numbers := dbInt(t, `SELECT id FROM tasks WHERE title = 'Numbers' AND user_id = $1`, ann.ID)
	bens := ben.task("Ben task", nil)
	done := func(task int64, on bool) {
		form := url.Values{}
		if on {
			form.Set("done", "1")
		}
		if res := ann.post("/tasks/"+id(task)+"/done", form); res.code != http.StatusSeeOther {
			t.Fatalf("done %d: %d", task, res.code)
		}
	}
	isDone := func(task int64) bool {
		return dbInt(t, `SELECT count(*) FROM tasks WHERE id = $1 AND done_at IS NOT NULL`, task) == 1
	}
	lists := func() string {
		return fmt.Sprint(ann.taskTitles(""), ann.taskTitles("?view=today"), ann.taskTitles("?view=completed"))
	}

	done(report, true)
	if isDone(report) {
		t.Error("a task outside Today was checked off")
	}
	ann.post("/tasks/today", url.Values{"id": {id(report), id(bills), id(bens), id(numbers)}})
	if n := dbInt(t, `SELECT count(*) FROM tasks WHERE today_on IS NOT NULL AND id IN ($1, $2)`, bens, numbers); n != 0 {
		t.Error("moved another user's task or a subtask to Today")
	}
	if got := lists(); got != "[Report Bills Gym] [Report Bills] []" {
		t.Errorf("after moving to Today: %s", got)
	}
	if body := ann.get("/tasks?view=today").body; !strings.Contains(body, "Summary") {
		t.Error("Today should list subtasks under their task")
	}

	done(numbers, true) // subtasks check off anytime
	done(report, true)  // and checking the task completes the rest
	if !isDone(report) || dbInt(t, `SELECT count(*) FROM tasks WHERE parent_id = $1 AND done_at IS NULL`, report) != 0 {
		t.Error("checking a task should complete it and its subtasks")
	}
	if got := lists(); got != "[Bills Gym] [Bills Report] []" {
		t.Errorf("after checking Report: %s", got)
	}

	// the next day: checked tasks are history, unchecked ones are back in the main list
	testDB.Exec(t.Context(), `UPDATE tasks SET today_on = today_on - 1 WHERE user_id = $1`, ann.ID)
	if got := lists(); got != "[Bills Gym] [] [Report]" {
		t.Errorf("next day: %s", got)
	}
	if body := ann.get("/tasks?view=completed").body; !strings.Contains(body, "Yesterday") {
		t.Error("Completed should group by day")
	}
	done(bills, true)
	if isDone(bills) {
		t.Error("yesterday's unchecked task was checked off outside Today")
	}

	// a task added on the Today list goes into it; one taken out goes back
	ann.task("Call mom", url.Values{"view": {"today"}})
	ann.post("/tasks/today", url.Values{"id": {id(gym)}})
	ann.post("/tasks/today", url.Values{"id": {id(gym)}, "remove": {"1"}})
	if got := fmt.Sprint(ann.taskTitles("?view=today")); got != "[Call mom]" {
		t.Errorf("Today: %s", got)
	}
	// reopening a completed task puts it back in the main list
	done(report, false)
	if got := fmt.Sprint(ann.taskTitles("")); got != "[Report Bills Gym Call mom]" {
		t.Errorf("after reopening: %s", got)
	}
}
