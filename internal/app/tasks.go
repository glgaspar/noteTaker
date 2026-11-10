package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
		JOIN task_tags g ON g.id = l.tag_id WHERE l.task_id = t.id), '{}')
	FROM tasks t LEFT JOIN tasks p ON p.id = t.parent_id`

// taskList lists the user's top-level tasks, or with tagID every task with that tag,
// soonest due first.
func (s *Server) taskList(ctx context.Context, userID, tagID int64) ([]task, error) {
	rows, err := s.db.Query(ctx, `SELECT `+taskColumns+`
		WHERE t.user_id = $1 AND CASE WHEN $2::bigint = 0 THEN t.parent_id IS NULL
			ELSE EXISTS (SELECT 1 FROM task_tag_links l WHERE l.task_id = t.id AND l.tag_id = $2) END
		ORDER BY t.due_on NULLS LAST, t.created_at, t.id`, userID, tagID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[task])
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

// tasksURL is the task page, keeping the tag filter.
func tasksURL(tagID, taskID int64) string {
	q := url.Values{}
	if tagID != 0 {
		q.Set("tag", fmt.Sprint(tagID))
	}
	if taskID != 0 {
		q.Set("task", fmt.Sprint(taskID))
	}
	if len(q) == 0 {
		return "/tasks"
	}
	return "/tasks?" + q.Encode()
}

func (s *Server) tasksPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := currentUser(r)
	tagID, taskID := orZero(formID(r, "tag")), orZero(formID(r, "task"))
	data := map[string]any{"User": user, "TagID": tagID, "Loc": userLoc(r), "Collapsed": collapsedPanes(r), "ListTitle": "All tasks"}
	var err error
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
	if data["Tasks"], err = s.taskList(ctx, user.ID, tagID); err != nil {
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
	tagID := orZero(formID(r, "tag"))
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		redirect(w, r, tasksURL(tagID, 0))
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
		err = s.db.QueryRow(ctx, `INSERT INTO tasks (user_id, title, due_on) VALUES ($1, $2, $3) RETURNING id`,
			userID, title, parseDue(r.FormValue("due_on"))).Scan(&id)
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
		redirect(w, r, tasksURL(tagID, *parentID))
		return
	}
	redirect(w, r, tasksURL(tagID, id))
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
	redirect(w, r, tasksURL(orZero(formID(r, "tag")), id))
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
	redirect(w, r, tasksURL(orZero(formID(r, "tag")), parentID))
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
	redirect(w, r, tasksURL(orZero(formID(r, "tag")), id))
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
	redirect(w, r, tasksURL(orZero(formID(r, "tag")), id))
}
