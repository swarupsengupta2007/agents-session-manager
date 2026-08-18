// Package migrate plans and executes transcript migrations (remaps and
// soft deletes). Every mutating action first copies the original artifact
// into a timestamped backup directory, so nothing is ever destroyed.
package migrate

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"
)

type ActionKind int

const (
	RewriteCwd ActionKind = iota // rewrite "<Field>": <json(Old)> -> <json(New)> inside file Src (Field defaults to "cwd")
	MoveFile
	MoveDir
	RemoveEmptyDir     // remove Src only if it is an empty directory
	Archive            // soft delete: move Src into the backup dir
	SQLiteSetCwd       // Src = db path, SessionID selects the row, New = new cwd (Table defaults to session_docs)
	SQLiteDelete       // Src = db path, delete the row for SessionID
	SQLiteSetWorkspace // Src = db path, rewrite file://<Old> -> file://<New> inside workspace_uris of the SessionID row
	ProjectsJSONRemap  // Src = projects.json path, move the map key Old -> New
	SetJSONKey         // set/insert string Field in JSON object file Src
	AppendJSONL        // append New as one JSONL line to Src
	SQLiteMuseRename   // sessions.title + session_name + increment session_name_revision
	WriteFile          // write New to Dst (or Src if Dst empty)
)

// Action is one atomic, describable mutation.
type Action struct {
	Kind      ActionKind
	Src, Dst  string
	Old, New  string
	SessionID string
	Field     string // JSON field for RewriteCwd; defaults to "cwd"
	Table     string // sqlite table for the SQLite* kinds; defaults to session_docs
	Column    string // sqlite key column for the SQLite* kinds; defaults to session_id
	SetColumn string // column SET by SQLiteSetCwd; defaults to cwd
	Desc      string
}

// Plan is an ordered list of actions for one agent, plus the cwd move it
// implements (empty OldCwd/NewCwd for delete plans).
type Plan struct {
	Agent          string
	OldCwd, NewCwd string
	NewTitle       string // set by rename plans
	Actions        []Action
}

func (p *Plan) Add(a Action) { p.Actions = append(p.Actions, a) }
func (p *Plan) Empty() bool  { return len(p.Actions) == 0 }

// Validate checks remap preconditions. Delete plans (no NewCwd) pass.
func (p *Plan) Validate() error {
	if p.NewCwd == "" {
		return nil
	}
	if filepath.Clean(p.NewCwd) == filepath.Clean(p.OldCwd) {
		return errors.New("old and new paths are identical")
	}
	st, err := os.Stat(p.NewCwd)
	if err != nil {
		return fmt.Errorf("target path: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("target path %q is not a directory", p.NewCwd)
	}
	return nil
}

// Report is the outcome of an Apply run.
type Report struct {
	BackupDir string
	Done      []string
	Warnings  []string // post-write lock or delta mismatches; apply still finished
}

// ProbeFunc is called around each mutation. A non-nil error aborts the
// action before the original is replaced. After a successful replace it
// becomes a warning (the bytes are already swapped).
type ProbeFunc func() error

// Apply executes the plan with no live-agent probe.
func Apply(p *Plan, backupRoot string) (*Report, error) {
	return ApplyWith(p, backupRoot, nil)
}

// RewriteCwdInFile replaces every "cwd": <json(old)> occurrence with the new
// value. Kept as a convenience wrapper around RewriteFieldInFile.
func RewriteCwdInFile(path, oldCwd, newCwd string) error {
	return RewriteFieldInFile(path, "cwd", oldCwd, newCwd)
}

// RewriteFieldInFile replaces every "<field>": <json(old)> occurrence with
// the new value, preserving surrounding whitespace style (compact and pretty
// JSON). Only exact JSON string values match, so "/a/b" never clobbers
// "/a/b10". A file without matches is left untouched.
func RewriteFieldInFile(path, field, oldVal, newVal string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	oldJSON, err := json.Marshal(oldVal)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal(newVal)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`("` + regexp.QuoteMeta(field) + `"\s*:\s*)` + regexp.QuoteMeta(string(oldJSON)))
	if !re.Match(in) {
		return nil
	}
	out := re.ReplaceAllFunc(in, func(m []byte) []byte {
		idx := bytes.Index(m, oldJSON)
		b := make([]byte, 0, idx+len(newJSON))
		b = append(b, m[:idx]...)
		return append(b, newJSON...)
	})
	if err := os.WriteFile(path, out, info.Mode().Perm()); err != nil {
		return err
	}
	// Keep the original mtime so "last updated" columns stay honest.
	return os.Chtimes(path, info.ModTime(), info.ModTime())
}

func sqliteMutate(a Action) error {
	db, err := sql.Open("sqlite", a.Src+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	table, err := sqlIdent(a.Table, "session_docs")
	if err != nil {
		return err
	}
	col, err := sqlIdent(a.Column, "session_id")
	if err != nil {
		return err
	}
	switch a.Kind {
	case SQLiteSetCwd:
		setCol, serr := sqlIdent(a.SetColumn, "cwd")
		if serr != nil {
			return serr
		}
		_, err = db.Exec(`UPDATE `+table+` SET `+setCol+` = ? WHERE `+col+` = ?`, a.New, a.SessionID)
	case SQLiteDelete:
		_, err = db.Exec(`DELETE FROM `+table+` WHERE `+col+` = ?`, a.SessionID)
	case SQLiteSetWorkspace:
		err = sqliteSetWorkspace(db, a.SessionID, a.Old, a.New)
	case SQLiteMuseRename:
		err = sqliteMuseRename(db, table, col, a.New, a.SessionID)
	}
	return err
}

func sqliteMuseRename(db *sql.DB, table, col, title, sessionID string) error {
	_, err := db.Exec(`UPDATE `+table+` SET title = ?, session_name = ?, session_name_revision = COALESCE(session_name_revision, 0) + 1 WHERE `+col+` = ?`,
		title, title, sessionID)
	if err != nil && strings.Contains(err.Error(), "no such column") {
		_, err = db.Exec(`UPDATE `+table+` SET title = ? WHERE `+col+` = ?`, title, sessionID)
	}
	return err
}

// SetJSONStringField sets key to val in a JSON object file.
func SetJSONStringField(path, key, val string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(in, &obj); err != nil {
		return err
	}
	obj[key] = val
	var out []byte
	if bytes.Contains(in, []byte("\n")) {
		out, err = json.MarshalIndent(obj, "", "  ")
	} else {
		out, err = json.Marshal(obj)
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(path, info.ModTime(), info.ModTime())
}

func appendJSONL(path, line string) error {
	line = strings.TrimRight(line, "\n") + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// sqlIdent allows only letters, digits and underscore so table/column names
// interpolated into SQL cannot inject statements.
func sqlIdent(s, fallback string) (string, error) {
	if s == "" {
		s = fallback
	}
	if s == "" {
		return "", fmt.Errorf("empty SQL identifier")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return "", fmt.Errorf("invalid SQL identifier %q", s)
		}
	}
	return s, nil
}

// sqliteSetWorkspace rewrites file://<old> to file://<new> inside the
// workspace_uris JSON array of one conversation_summaries row.
func sqliteSetWorkspace(db *sql.DB, conversationID, oldCwd, newCwd string) error {
	var uris string
	err := db.QueryRow(`SELECT workspace_uris FROM conversation_summaries WHERE conversation_id = ?`,
		conversationID).Scan(&uris)
	if err == sql.ErrNoRows {
		return nil // conversation has no summary row; nothing to rewrite
	}
	if err != nil {
		return err
	}
	oldJSON, err := json.Marshal("file://" + oldCwd)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal("file://" + newCwd)
	if err != nil {
		return err
	}
	updated := strings.ReplaceAll(uris, string(oldJSON), string(newJSON))
	if updated == uris {
		return nil
	}
	_, err = db.Exec(`UPDATE conversation_summaries SET workspace_uris = ? WHERE conversation_id = ?`,
		updated, conversationID)
	return err
}

// rewriteProjectsJSON moves the map entry keyed by oldPath to newPath in an
// agy projects.json file ({ "projects": { "<path>": "<name>" } }).
func rewriteProjectsJSON(path, oldPath, newPath string) error {
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Values may be strings (agy projects.json) or objects (muse trust.json).
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(in, &doc); err != nil {
		return err
	}
	raw, ok := doc["projects"]
	if !ok {
		return nil
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(raw, &projects); err != nil {
		return err
	}
	old := filepath.Clean(oldPath)
	neu := filepath.Clean(newPath)
	val, ok := projects[old]
	if !ok {
		return nil // old path not registered; the agent re-registers on next run
	}
	if _, exists := projects[neu]; exists {
		return fmt.Errorf("projects.json already has an entry for %s", newPath)
	}
	delete(projects, old)
	projects[neu] = val
	b, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	doc["projects"] = b
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func removeIfEmpty(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	// Non-empty directory: leave it. Check the dir rather than errno so
	// this works on Windows (no ENOTEMPTY) as well as Unix.
	if ents, rerr := os.ReadDir(path); rerr == nil && len(ents) > 0 {
		return nil
	}
	return err
}

func movePath(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && linkErr.Err == syscall.EXDEV {
		// Cross-device: fall back to copy + remove.
		st, statErr := os.Stat(src)
		if statErr != nil {
			return statErr
		}
		if st.IsDir() {
			if err := copyDir(src, dst); err != nil {
				return err
			}
		} else {
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
		return os.RemoveAll(src)
	}
	return err
}

// backupPath copies a file or directory tree into the backup dir, mirroring
// its path relative to $HOME. Existing destinations get a numeric suffix.
func backupPath(backupDir, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	dst := uniquePath(filepath.Join(backupDir, relForBackup(path)))
	if st.IsDir() {
		return copyDir(path, dst)
	}
	return copyFile(path, dst)
}

func relForBackup(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

func uniquePath(path string) string {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s~%d%s", base, i, ext)
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return copyFile(p, target)
	})
}
