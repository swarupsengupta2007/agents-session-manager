package agents

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"

	_ "modernc.org/sqlite"
)

func (c *Claude) ImportPlan(t *Transcript, newID string) (*migrate.Plan, error) {
	t.ensureMeta()
	dir := filepath.Join(c.projectsDir(), EncodePath(t.Cwd))
	path := filepath.Join(dir, newID+".jsonl")
	var b strings.Builder
	b.WriteString(mustJSONLine(map[string]any{
		"type": "system", "cwd": t.Cwd, "sessionId": newID, "timestamp": rfc3339(t.Created),
	}))
	var prev string
	for i, m := range t.Messages {
		id := newSessionID()
		rec := map[string]any{
			"type": m.Role, "cwd": t.Cwd, "sessionId": newID,
			"timestamp": rfc3339(msgTime(t, i)), "uuid": id,
		}
		if prev != "" {
			rec["parentUuid"] = prev
		}
		prev = id
		if m.Role == "assistant" {
			msg := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": m.Text}}}
			if m.Model != "" {
				msg["model"] = m.Model
				rec["model"] = m.Model
			} else if t.Model != "" {
				msg["model"] = t.Model
				rec["model"] = t.Model
			}
			rec["message"] = msg
		} else {
			rec["message"] = map[string]any{"role": "user", "content": m.Text}
		}
		b.WriteString(mustJSONLine(rec))
	}
	if t.Title != "" {
		b.WriteString(mustJSONLine(map[string]any{
			"type": "ai-title", "aiTitle": t.Title, "sessionId": newID,
		}))
	}
	plan := &migrate.Plan{Agent: string(model.Claude)}
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: path, New: b.String(),
		Desc: fmt.Sprintf("write claude transcript %s", filepath.Base(path))})
	return plan, nil
}

func (c *Codex) ImportPlan(t *Transcript, newID string) (*migrate.Plan, error) {
	t.ensureMeta()
	ts := t.Created
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	dir := filepath.Join(c.sessionsDir(), ts.UTC().Format("2006"), ts.UTC().Format("01"), ts.UTC().Format("02"))
	stamp := ts.UTC().Format("2006-01-02T15-04-05")
	path := filepath.Join(dir, "rollout-"+stamp+"-"+newID+".jsonl")
	var b strings.Builder
	b.WriteString(mustJSONLine(map[string]any{
		"timestamp": rfc3339(ts),
		"type":      "session_meta",
		"payload": map[string]any{
			"id": newID, "session_id": newID, "cwd": t.Cwd,
			"timestamp": rfc3339(ts), "originator": "agents-session-manager",
		},
	}))
	for i, m := range t.Messages {
		kind := "user_message"
		if m.Role == "assistant" {
			kind = "agent_message"
		}
		b.WriteString(mustJSONLine(map[string]any{
			"timestamp": rfc3339(msgTime(t, i)),
			"type":      "event_msg",
			"payload":   map[string]any{"type": kind, "message": m.Text},
		}))
	}
	plan := &migrate.Plan{Agent: string(model.Codex)}
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: path, New: b.String(),
		Desc: fmt.Sprintf("write codex rollout %s", filepath.Base(path))})
	if t.Title != "" {
		plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: path + ".title", New: t.Title + "\n",
			Desc: fmt.Sprintf("write title sidecar for %s", newID)})
	}
	return plan, nil
}

func (g *Grok) ImportPlan(t *Transcript, newID string) (*migrate.Plan, error) {
	t.ensureMeta()
	dir := filepath.Join(g.sessionsDir(), encodeDir(t.Cwd), newID)
	var hist strings.Builder
	for _, m := range t.Messages {
		if m.Role == "assistant" {
			hist.WriteString(mustJSONLine(map[string]any{"type": "assistant", "content": m.Text}))
		} else {
			hist.WriteString(mustJSONLine(map[string]any{
				"type": "user", "content": []any{map[string]any{"type": "text", "text": m.Text}},
			}))
		}
	}
	n := len(t.Messages)
	sum := grokSummary{
		SessionSummary:  t.Title,
		CreatedAt:       rfc3339(t.Created),
		UpdatedAt:       rfc3339(t.Updated),
		LastActiveAt:    rfc3339(t.Updated),
		NumMessages:     n,
		NumChatMessages: n,
		CurrentModelID:  t.Model,
	}
	sum.Info.ID = newID
	sum.Info.Cwd = t.Cwd
	plan := &migrate.Plan{Agent: string(model.Grok)}
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: filepath.Join(dir, "summary.json"),
		New: prettyJSON(sum), Desc: fmt.Sprintf("write grok summary %s", newID)})
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: filepath.Join(dir, "chat_history.jsonl"),
		New: hist.String(), Desc: fmt.Sprintf("write grok chat_history %s", newID)})
	if _, err := os.Stat(g.sqlitePath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.SQLiteJSONUpsert, Src: g.sqlitePath(), SessionID: newID,
			Table: "session_docs", Column: "session_id",
			New:  mustJSON(map[string]any{"session_id": newID, "cwd": t.Cwd, "title": t.Title}),
			Desc: fmt.Sprintf("index grok session %s", newID),
		})
	}
	return plan, nil
}

func (a *Agy) ImportPlan(t *Transcript, newID string) (*migrate.Plan, error) {
	t.ensureMeta()
	dbBytes, err := agyMinimalDB(len(t.Messages))
	if err != nil {
		return nil, err
	}
	plan := &migrate.Plan{Agent: string(model.Agy)}
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: a.convDB(newID), New: string(dbBytes),
		Desc: fmt.Sprintf("write agy conversation db %s", newID)})
	line, _ := json.Marshal(map[string]any{
		"display": t.Title, "workspace": t.Cwd, "conversationId": newID,
		"timestamp": t.Updated.UTC().UnixMilli(),
	})
	plan.Add(migrate.Action{Kind: migrate.AppendJSONL, Src: a.historyPath(), New: string(line),
		Desc: fmt.Sprintf("append agy history for %s", newID)})
	if _, err := os.Stat(a.summariesPath()); err == nil {
		uris, _ := json.Marshal([]string{"file://" + t.Cwd})
		plan.Add(migrate.Action{
			Kind: migrate.SQLiteJSONUpsert, Src: a.summariesPath(), SessionID: newID,
			Table: "conversation_summaries", Column: "conversation_id",
			New: mustJSON(map[string]any{
				"conversation_id": newID, "title": t.Title, "preview": t.Title,
				"step_count": len(t.Messages), "last_modified_time": rfc3339(t.Updated),
				"workspace_uris": string(uris),
			}),
			Desc: fmt.Sprintf("index agy conversation %s", newID),
		})
	}
	return plan, nil
}

func (q *Qwen) ImportPlan(t *Transcript, newID string) (*migrate.Plan, error) {
	t.ensureMeta()
	dir := filepath.Join(q.projectsDir(), EncodePath(t.Cwd), "chats")
	path := filepath.Join(dir, newID+".jsonl")
	rt := filepath.Join(dir, newID+".runtime.json")
	var b strings.Builder
	var prev string
	for i, m := range t.Messages {
		id := newSessionID()
		rec := map[string]any{
			"uuid": id, "sessionId": newID, "cwd": t.Cwd,
			"timestamp": rfc3339(msgTime(t, i)), "type": m.Role,
		}
		if prev != "" {
			rec["parentUuid"] = prev
		}
		prev = id
		if m.Role == "assistant" {
			modelName := m.Model
			if modelName == "" {
				modelName = t.Model
			}
			if modelName != "" {
				rec["model"] = modelName
			}
			rec["message"] = map[string]any{"role": "model", "parts": []any{map[string]any{"text": m.Text}}}
		} else {
			rec["message"] = map[string]any{"role": "user", "parts": []any{map[string]any{"text": m.Text}}}
		}
		b.WriteString(mustJSONLine(rec))
	}
	rtBody, _ := json.Marshal(map[string]any{
		"schema_version": 1, "pid": 0, "session_id": newID, "work_dir": t.Cwd, "title": t.Title,
	})
	plan := &migrate.Plan{Agent: string(model.Qwen)}
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: path, New: b.String(),
		Desc: fmt.Sprintf("write qwen transcript %s", filepath.Base(path))})
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: rt, New: string(rtBody) + "\n",
		Desc: fmt.Sprintf("write qwen runtime %s", newID)})
	return plan, nil
}

func (m *Muse) ImportPlan(t *Transcript, newID string) (*migrate.Plan, error) {
	t.ensureMeta()
	ts := t.Created
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	dir := filepath.Join(m.sessionsDir(), ts.UTC().Format("2006"), ts.UTC().Format("01"), ts.UTC().Format("02"), newID)
	log := filepath.Join(dir, "session.jsonl")
	var b strings.Builder
	seq := 1
	write := func(payloadType string, payload any, at time.Time) {
		if at.IsZero() {
			at = ts
		}
		b.WriteString(mustJSONLine(map[string]any{
			"schema_version": 1, "id": newSessionID(),
			"stream":   map[string]any{"kind": "session", "id": newID},
			"sequence": seq, "recorded_at": at.UnixMicro(),
			"payload_type": payloadType, "payload": payload,
		}))
		seq++
	}
	write("runtime.session.metadata", map[string]any{
		"kind": "metadata", "record": map[string]any{"model_id": t.Model, "workspace_root": t.Cwd},
	}, t.Created)
	write("runtime.session.route_facts", map[string]any{
		"kind": "route_facts", "record": map[string]any{"cwd": t.Cwd, "pid": 0},
	}, t.Created)
	for i, msg := range t.Messages {
		if msg.Role != "user" {
			// Muse scan counts user_intent turns; keep assistant text as a user-visible
			// follow-up intent so it is not dropped on a later re-export.
			if msg.Role == "assistant" {
				write("runtime.user_intent.accepted", map[string]any{
					"model_messages": []any{map[string]any{
						"content": []any{map[string]any{"kind": "text", "text": "[assistant] " + msg.Text}},
					}},
				}, msgTime(t, i))
			}
			continue
		}
		write("runtime.user_intent.accepted", map[string]any{
			"model_messages": []any{map[string]any{
				"content": []any{map[string]any{"kind": "text", "text": msg.Text}},
			}},
		}, msgTime(t, i))
	}
	plan := &migrate.Plan{Agent: string(model.Muse)}
	plan.Add(migrate.Action{Kind: migrate.WriteFile, Src: log, New: b.String(),
		Desc: fmt.Sprintf("write muse session %s", newID)})
	if _, err := os.Stat(m.indexPath()); err == nil {
		plan.Add(migrate.Action{
			Kind: migrate.SQLiteJSONUpsert, Src: m.indexPath(), SessionID: newID,
			Table: "sessions", Column: "session_id",
			New: mustJSON(map[string]any{
				"session_id": newID, "session_stream_id": newID,
				"session_dir": dir, "session_log_path": log,
				"layout": "session_jsonl", "workspace_root": t.Cwd, "workspace_key": t.Cwd,
				"model_id": t.Model, "title": t.Title, "session_name": t.Title,
				"prompt_count": len(t.Messages),
			}),
			Desc: fmt.Sprintf("index muse session %s", newID),
		})
	}
	return plan, nil
}

func msgTime(t *Transcript, i int) time.Time {
	if i >= 0 && i < len(t.Messages) && !t.Messages[i].Time.IsZero() {
		return t.Messages[i].Time
	}
	if !t.Created.IsZero() {
		return t.Created.Add(time.Duration(i) * time.Second)
	}
	return time.Now().UTC()
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mustJSON(v) + "\n"
	}
	return string(b) + "\n"
}

func agyMinimalDB(steps int) ([]byte, error) {
	if steps < 1 {
		steps = 1
	}
	dir, err := os.MkdirTemp("", "asm-agy-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "conv.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx INTEGER)`); err != nil {
		db.Close()
		return nil, err
	}
	for i := 0; i < steps; i++ {
		if _, err := db.Exec(`INSERT INTO steps (idx) VALUES (?)`, i); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
