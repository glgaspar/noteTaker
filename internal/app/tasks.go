package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tasks are each user's private to-do list (see migrations/014_tasks.sql). Every query
// filters on the current user, so nothing here is shared.

type task struct {
	ID          int64
	ParentID    int64
	Title       string
	Details     string
	DueOn       *time.Time
	ParentTitle string
	Subtasks    int
	Tags        []string
	TodayOn     *time.Time
	Done        bool
	Subs        []task `db:"-"` // subtasks, on the Today list
}

// localToday is the viewer's date, from the time zone their browser reports.
func localToday(r *http.Request) time.Time {
	y, m, d := time.Now().In(userLoc(r)).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (t task) InToday(today time.Time) bool { return t.TodayOn != nil && t.TodayOn.Equal(today) }

// DayLabel names the day a completed task was on Today: "Yesterday", "Mon, Nov 9", "Mon, Nov 9, 2025".
func (t task) DayLabel(today time.Time) string {
	switch {
	case t.TodayOn.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case t.TodayOn.Year() == today.Year():
		return t.TodayOn.Format("Mon, Jan 2")
	}
	return t.TodayOn.Format("Mon, Jan 2, 2006")
}

// dueIn is the number of days from today, in the viewer's zone, to the due date.
func (t task) dueIn(loc *time.Location) int {
	y, m, d := time.Now().In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	due := time.Date(t.DueOn.Year(), t.DueOn.Month(), t.DueOn.Day(), 0, 0, 0, 0, time.UTC)
	return int(due.Sub(today).Hours() / 24)
}

func (t task) Overdue(loc *time.Location) bool { return t.DueOn != nil && t.dueIn(loc) < 0 }

// DueLabel formats the due date: "Today", "Tomorrow", "Yesterday", "Fri", "Mar 4", "Mar 4, 2027".
func (t task) DueLabel(loc *time.Location) string {
	switch days := t.dueIn(loc); {
	case days == 0:
		return "Today"
	case days == 1:
		return "Tomorrow"
	case days == -1:
		return "Yesterday"
	case days > 1 && days < 7:
		return t.DueOn.Format("Mon")
	case t.DueOn.Year() == time.Now().In(loc).Year():
		return t.DueOn.Format("Jan 2")
	}
	return t.DueOn.Format("Jan 2, 2006")
}

const taskColumns = `
	t.id, coalesce(t.parent_id, 0), t.title, t.details, t.due_on, coalesce(p.title, ''),
	(SELECT count(*)::int FROM tasks s WHERE s.parent_id = t.id),
	coalesce((SELECT array_agg(g.name ORDER BY lower(g.name)) FROM task_tag_links l
		JOIN task_tags g ON g.id = l.tag_id WHERE l.task_id = t.id), '{}'),
	t.today_on, t.done_at IS NOT NULL
	FROM tasks t LEFT JOIN tasks p ON p.id = t.parent_id`

// The task lists, by the "view" parameter, with @user, @today (the viewer's date) and @tag.
var taskViews = map[string]string{
	// unfinished top-level tasks, or with a tag every unfinished task with that tag; soonest due first
	"": `t.done_at IS NULL AND CASE WHEN @tag::bigint = 0 THEN t.parent_id IS NULL
			ELSE EXISTS (SELECT 1 FROM task_tag_links l WHERE l.task_id = t.id AND l.tag_id = @tag) END
		ORDER BY t.due_on NULLS LAST, t.created_at, t.id`,
	// moved to Today today, done or not
	"today": `t.parent_id IS NULL AND t.today_on = @today
		ORDER BY t.done_at IS NOT NULL, t.due_on NULLS LAST, t.created_at, t.id`,
	// done on an earlier day, newest day first
	"completed": `t.parent_id IS NULL AND t.done_at IS NOT NULL AND t.today_on < @today
		ORDER BY t.today_on DESC, t.done_at DESC, t.id`,
}

func (s *Server) taskList(ctx context.Context, userID int64, today time.Time, view string, tagID int64) ([]task, error) {
	args := pgx.NamedArgs{"user": userID, "today": today, "tag": tagID}
	rows, err := s.db.Query(ctx, `SELECT `+taskColumns+` WHERE t.user_id = @user AND `+taskViews[view], args)
	if err != nil {
		return nil, err
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[task])
	if err != nil || view != "today" {
		return tasks, err
	}
	// Today shows subtasks under their tasks, to check them off.
	rows, err = s.db.Query(ctx, `SELECT `+taskColumns+`
		WHERE t.user_id = $1 AND p.today_on = $2 ORDER BY t.created_at, t.id`, userID, today)
	if err != nil {
		return nil, err
	}
	subs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[task])
	for _, sub := range subs {
		for i := range tasks {
			if tasks[i].ID == sub.ParentID {
				tasks[i].Subs = append(tasks[i].Subs, sub)
			}
		}
	}
	return tasks, err
}

func (s *Server) subtasks(ctx context.Context, userID, parentID int64) ([]task, error) {
	rows, err := s.db.Query(ctx, `SELECT `+taskColumns+`
		WHERE t.user_id = $1 AND t.parent_id = $2 ORDER BY t.created_at, t.id`, userID, parentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[task])
}

// loadTask returns the user's task, or nil when it does not exist or is someone else's.
func (s *Server) loadTask(ctx context.Context, userID, id int64) (*task, error) {
	rows, err := s.db.Query(ctx, `SELECT `+taskColumns+` WHERE t.user_id = $1 AND t.id = $2`, userID, id)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[task])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// taskTags lists the user's tags in use, with their task counts; with taskID only that task's.
func (s *Server) taskTags(ctx context.Context, userID, taskID int64) ([]tag, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id, g.name, count(*)::int FROM task_tags g JOIN task_tag_links l ON l.tag_id = g.id
		WHERE g.user_id = $1 AND ($2::bigint = 0 OR l.task_id = $2)
		GROUP BY g.id ORDER BY lower(g.name)`, userID, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[tag])
}

// taskView is the list asked for in the request ("", "today" or "completed"); only the main
// list filters by tag.
func taskView(r *http.Request) (view string, tagID int64) {
	view = r.FormValue("view")
	if _, ok := taskViews[view]; !ok && view != "inbox" {
		view = ""
	}
	if view == "" {
		tagID = orZero(formID(r, "tag"))
	}
	return view, tagID
}

// tasksURL is the task page with taskID open, keeping the request's list.
func tasksURL(r *http.Request, taskID int64) string {
	view, tagID := taskView(r)
	q := url.Values{}
	if view != "" {
		q.Set("view", view)
	}
	if tagID != 0 {
		q.Set("tag", fmt.Sprint(tagID))
	}
	query := q.Encode()
	if taskID != 0 { // last, as in the page's links
		query = strings.TrimPrefix(fmt.Sprintf("%s&task=%d", query, taskID), "&")
	}
	if query == "" {
		return "/tasks"
	}
	return "/tasks?" + query
}

func (s *Server) tasksPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	view, tagID := taskView(r)
	taskID, today := orZero(formID(r, "task")), localToday(r)
	data := map[string]any{"User": user, "View": view, "TagID": tagID, "Today": today, "Loc": userLoc(r),
		"Collapsed": collapsedPanes(r), "ListTitle": map[string]string{"": "All tasks", "today": "Today", "completed": "Completed", "inbox": "Inbox"}[view]}
	var err error
	var todayCount, inboxCount int
	if err := s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM tasks WHERE user_id = $1 AND parent_id IS NULL AND today_on = $2 AND done_at IS NULL),
		(SELECT count(*) FROM inbox_items WHERE user_id = $1 AND status = 'new')`,
		user.ID, today).Scan(&todayCount, &inboxCount); err != nil {
		s.serverError(w, r, err)
		return
	}
	data["TodayCount"], data["InboxCount"] = todayCount, inboxCount
	if data["Workspaces"], err = s.userWorkspaces(ctx, user.ID, 0); err != nil {
		s.serverError(w, r, err)
		return
	}
	tags, err := s.taskTags(ctx, user.ID, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, t := range tags {
		if t.ID == tagID {
			data["ListTitle"] = "#" + t.Name
		}
	}
	data["Tags"] = tags
	if view == "inbox" {
		data["Items"], err = s.inboxItems(ctx, user.ID)
	} else {
		data["Tasks"], err = s.taskList(ctx, user.ID, today, view, tagID)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if taskID != 0 {
		t, err := s.loadTask(ctx, user.ID, taskID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if t == nil {
			s.fail(w, r, http.StatusNotFound, "")
			return
		}
		data["Task"] = t
		if data["Subtasks"], err = s.subtasks(ctx, user.ID, t.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if data["TaskTags"], err = s.taskTags(ctx, user.ID, t.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.render(w, "tasks.html", data)
}

// ownTask checks the {id} task belongs to the current user; otherwise it answers 404.
func (s *Server) ownTask(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id := pathID(r, "id")
	var ok bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND user_id = $2)`,
		id, currentUser(r).ID).Scan(&ok); err != nil {
		s.serverError(w, r, err)
		return 0, false
	}
	if !ok {
		s.fail(w, r, http.StatusNotFound, "")
	}
	return id, ok
}

// parseDue reads a yyyy-mm-dd date input; empty or invalid means no due date.
func parseDue(v string) *time.Time {
	d, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return nil
	}
	return &d
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := currentUser(r).ID
	view, tagID := taskView(r)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		redirect(w, r, tasksURL(r, 0))
		return
	}
	parentID := formID(r, "parent_id")
	var id int64
	var err error
	if parentID != nil {
		// only under the user's own top-level tasks: subtasks are one level deep
		err = s.db.QueryRow(ctx, `INSERT INTO tasks (user_id, parent_id, title)
			SELECT $1, id, $3 FROM tasks WHERE id = $2 AND user_id = $1 AND parent_id IS NULL RETURNING id`,
			userID, *parentID, title).Scan(&id)
	} else {
		// a task added on the Today list goes straight into it
		var todayOn *time.Time
		if view == "today" {
			t := localToday(r)
			todayOn = &t
		}
		err = s.db.QueryRow(ctx, `INSERT INTO tasks (user_id, title, due_on, today_on) VALUES ($1, $2, $3, $4) RETURNING id`,
			userID, title, parseDue(r.FormValue("due_on")), todayOn).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, http.StatusNotFound, "")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A task created while a tag is shown gets that tag, so it shows up in the list.
	if tagID != 0 {
		if _, err := s.db.Exec(ctx, `INSERT INTO task_tag_links (user_id, task_id, tag_id)
			SELECT $1, $2, id FROM task_tags WHERE id = $3 AND user_id = $1`, userID, id, tagID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if parentID != nil {
		redirect(w, r, tasksURL(r, *parentID))
		return
	}
	redirect(w, r, tasksURL(r, id))
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownTask(w, r)
	if !ok {
		return
	}
	// An empty title keeps the old one.
	if _, err := s.db.Exec(r.Context(), `UPDATE tasks SET title = coalesce(nullif($1, ''), title), details = $2, due_on = $3
		WHERE id = $4 AND user_id = $5`, strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("details")),
		parseDue(r.FormValue("due_on")), id, currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, tasksURL(r, id))
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownTask(w, r)
	if !ok {
		return
	}
	var parentID int64
	if err := s.db.QueryRow(r.Context(), `DELETE FROM tasks WHERE id = $1 AND user_id = $2 RETURNING coalesce(parent_id, 0)`,
		id, currentUser(r).ID).Scan(&parentID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, tasksURL(r, parentID))
}

func (s *Server) addTaskTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownTask(w, r)
	if !ok {
		return
	}
	if name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.FormValue("name")), "#")); name != "" {
		if _, err := s.db.Exec(r.Context(), `
			WITH g AS (
				INSERT INTO task_tags (user_id, name) VALUES ($1, $2)
				ON CONFLICT (user_id, name) DO UPDATE SET name = excluded.name
				RETURNING id
			)
			INSERT INTO task_tag_links (user_id, task_id, tag_id) SELECT $1, $3, id FROM g ON CONFLICT DO NOTHING`,
			currentUser(r).ID, name, id); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	redirect(w, r, tasksURL(r, id))
}

func (s *Server) removeTaskTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownTask(w, r)
	if !ok {
		return
	}
	if _, err := s.db.Exec(r.Context(), `DELETE FROM task_tag_links WHERE task_id = $1 AND tag_id = $2 AND user_id = $3`,
		id, pathID(r, "tag"), currentUser(r).ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, tasksURL(r, id))
}

// moveToToday puts the selected unfinished top-level tasks ("id", repeated) into Today, or
// with "remove" takes them out.
func (s *Server) moveToToday(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var ids []int64
	for _, v := range r.Form["id"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	var today *time.Time
	if r.FormValue("remove") == "" {
		t := localToday(r)
		today = &t
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE tasks SET today_on = $1
		WHERE user_id = $2 AND id = ANY($3) AND parent_id IS NULL AND done_at IS NULL`,
		today, currentUser(r).ID, ids); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, tasksURL(r, orZero(formID(r, "task"))))
}

// setDone checks a task off ("done" set) or reopens it. A top-level task can only be checked
// while it is in Today, and checking it completes its subtasks; subtasks can be checked anytime.
func (s *Server) setDone(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownTask(w, r)
	if !ok {
		return
	}
	args := pgx.NamedArgs{"id": id, "user": currentUser(r).ID, "today": localToday(r)}
	query := `UPDATE tasks SET done_at = NULL WHERE id = @id AND user_id = @user`
	if r.FormValue("done") != "" {
		query = `WITH t AS (
				UPDATE tasks SET done_at = now() WHERE id = @id AND user_id = @user AND done_at IS NULL
				AND (parent_id IS NOT NULL OR today_on = @today) RETURNING id
			)
			UPDATE tasks SET done_at = now() WHERE parent_id IN (SELECT id FROM t) AND user_id = @user AND done_at IS NULL`
	}
	if _, err := s.db.Exec(r.Context(), query, args); err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, tasksURL(r, orZero(formID(r, "task"))))
}
