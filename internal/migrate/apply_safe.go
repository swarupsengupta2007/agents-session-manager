package migrate

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ApplyWith executes the plan using copy-modify-swap:
//
//  1. probe — refuse if an agent may be writing
//  2. snapshot the original onto a sibling work copy (and into the backup dir)
//  3. probe again
//  4. mutate the work copy only
//  5. probe, then atomically replace the original with the work copy
//  6. probe again
//  7. compare on-disk bytes to the work copy we swapped in
//
// A delta mismatch or a post-swap probe failure is recorded as a warning
// (possible concurrent writer / corruption), not a hard failure.
func ApplyWith(p *Plan, backupRoot string, probe ProbeFunc) (*Report, error) {
	rep := &Report{}
	if p == nil || len(p.Actions) == 0 {
		return rep, nil
	}
	stamp := time.Now().Format("20060102-150405")
	rep.BackupDir = filepath.Join(backupRoot, stamp+"-"+p.Agent)
	if err := os.MkdirAll(rep.BackupDir, 0o755); err != nil {
		return rep, err
	}
	for _, a := range p.Actions {
		warns, err := applyAction(a, rep.BackupDir, probe)
		rep.Warnings = append(rep.Warnings, warns...)
		if err != nil {
			return rep, fmt.Errorf("%s: %w", a.Desc, err)
		}
		rep.Done = append(rep.Done, a.Desc)
	}
	return rep, nil
}

func applyAction(a Action, backupDir string, probe ProbeFunc) ([]string, error) {
	switch a.Kind {
	case RewriteCwd:
		field := a.Field
		if field == "" {
			field = "cwd"
		}
		return applyInPlace(a.Src, backupDir, probe, func(work string) error {
			return RewriteFieldInFile(work, field, a.Old, a.New)
		})
	case SetJSONKey:
		field := a.Field
		if field == "" {
			field = "title"
		}
		return applyInPlace(a.Src, backupDir, probe, func(work string) error {
			return SetJSONStringField(work, field, a.New)
		})
	case ProjectsJSONRemap:
		return applyInPlace(a.Src, backupDir, probe, func(work string) error {
			return rewriteProjectsJSON(work, a.Old, a.New)
		})
	case AppendJSONL:
		return applyCreateOrInPlace(a.Src, backupDir, probe, func(work string) error {
			return appendJSONL(work, a.New)
		})
	case WriteFile:
		path := a.Dst
		if path == "" {
			path = a.Src
		}
		return applyCreateOrInPlace(path, backupDir, probe, func(work string) error {
			if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
				return err
			}
			return os.WriteFile(work, []byte(a.New), 0o644)
		})
	case SQLiteSetCwd, SQLiteDelete, SQLiteSetWorkspace, SQLiteMuseRename:
		return applySQLite(a, backupDir, probe)
	case MoveFile:
		return applyMove(a.Src, a.Dst, backupDir, probe, false)
	case MoveDir:
		return applyMove(a.Src, a.Dst, backupDir, probe, true)
	case Archive:
		dst := uniquePath(filepath.Join(backupDir, "archived", filepath.Base(a.Src)))
		st, err := os.Stat(a.Src)
		if err != nil {
			return nil, err
		}
		return applyMove(a.Src, dst, backupDir, probe, st.IsDir())
	case RemoveEmptyDir:
		return applyRemoveEmpty(a.Src, probe)
	default:
		return nil, fmt.Errorf("unknown action kind %d", a.Kind)
	}
}

func applyInPlace(orig, backupDir string, probe ProbeFunc, mutate func(work string) error) ([]string, error) {
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	work, err := snapshotToWork(orig, backupDir)
	if err != nil {
		return nil, err
	}
	defer os.Remove(work)
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	if err := mutate(work); err != nil {
		return nil, err
	}
	return swapAndVerify(work, orig, probe)
}

func applyCreateOrInPlace(orig, backupDir string, probe ProbeFunc, mutate func(work string) error) ([]string, error) {
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	_, statErr := os.Stat(orig)
	var work string
	var err error
	if statErr == nil {
		work, err = snapshotToWork(orig, backupDir)
		if err != nil {
			return nil, err
		}
	} else if os.IsNotExist(statErr) {
		work = siblingWorkPath(orig)
		if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
			return nil, err
		}
	} else {
		return nil, statErr
	}
	defer os.Remove(work)
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	if err := mutate(work); err != nil {
		return nil, err
	}
	return swapAndVerify(work, orig, probe)
}

func applySQLite(a Action, backupDir string, probe ProbeFunc) ([]string, error) {
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	work, err := snapshotSQLite(a.Src, backupDir)
	if err != nil {
		return nil, err
	}
	defer removeSQLite(work)
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	wa := a
	wa.Src = work
	if err := sqliteMutate(wa); err != nil {
		return nil, err
	}
	if err := checkpointSQLite(work); err != nil {
		return nil, err
	}
	return swapSQLiteAndVerify(work, a.Src, probe)
}

func applyMove(src, dst, backupDir string, probe ProbeFunc, dir bool) ([]string, error) {
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	work, err := snapshotToWork(src, backupDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	expected, err := checksum(work)
	if err != nil {
		return nil, err
	}
	if dir {
		if err := movePath(work, dst); err != nil {
			return nil, err
		}
	} else {
		if err := replaceFile(work, dst); err != nil {
			return nil, err
		}
	}
	var warns []string
	if err := callProbe(probe); err != nil {
		warns = append(warns, err.Error())
	}
	got, err := checksum(dst)
	if err != nil {
		return warns, err
	}
	if got != expected {
		warns = append(warns, deltaWarn(dst))
	}
	if err := os.RemoveAll(src); err != nil {
		return warns, err
	}
	return warns, nil
}

func applyRemoveEmpty(path string, probe ProbeFunc) ([]string, error) {
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	if err := removeIfEmpty(path); err != nil {
		return nil, err
	}
	if err := callProbe(probe); err != nil {
		return []string{err.Error()}, nil
	}
	return nil, nil
}

func swapAndVerify(work, orig string, probe ProbeFunc) ([]string, error) {
	expected, err := checksum(work)
	if err != nil {
		return nil, err
	}
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	if err := replaceFile(work, orig); err != nil {
		return nil, err
	}
	var warns []string
	if err := callProbe(probe); err != nil {
		warns = append(warns, err.Error())
	}
	got, err := checksum(orig)
	if err != nil {
		return warns, err
	}
	if got != expected {
		warns = append(warns, deltaWarn(orig))
	}
	return warns, nil
}

func swapSQLiteAndVerify(work, orig string, probe ProbeFunc) ([]string, error) {
	expected, err := checksum(work)
	if err != nil {
		return nil, err
	}
	if err := callProbe(probe); err != nil {
		return nil, err
	}
	if err := replaceFile(work, orig); err != nil {
		return nil, err
	}
	removeSQLiteSidecars(orig)
	removeSQLiteSidecars(work)
	var warns []string
	if err := callProbe(probe); err != nil {
		warns = append(warns, err.Error())
	}
	got, err := checksum(orig)
	if err != nil {
		return warns, err
	}
	if got != expected {
		warns = append(warns, deltaWarn(orig))
	}
	return warns, nil
}

func callProbe(probe ProbeFunc) error {
	if probe == nil {
		return nil
	}
	return probe()
}

func deltaWarn(path string) string {
	return fmt.Sprintf("potential corruption: %s changed after write (on-disk delta != expected)", path)
}

func siblingWorkPath(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	return filepath.Join(dir, fmt.Sprintf(".asm-%s.%d.work", base, time.Now().UnixNano()))
}

func snapshotToWork(orig, backupDir string) (string, error) {
	st, err := os.Stat(orig)
	if err != nil {
		return "", err
	}
	work := siblingWorkPath(orig)
	if st.IsDir() {
		if err := copyDir(orig, work); err != nil {
			return "", err
		}
	} else {
		if err := copyFile(orig, work); err != nil {
			return "", err
		}
	}
	if err := backupFrom(backupDir, orig, work); err != nil {
		os.RemoveAll(work)
		return "", err
	}
	return work, nil
}

func backupFrom(backupDir, logical, src string) error {
	dst := uniquePath(filepath.Join(backupDir, relForBackup(logical)))
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst)
}

func snapshotSQLite(orig, backupDir string) (string, error) {
	work := siblingWorkPath(orig)
	if err := copySQLite(orig, work); err != nil {
		return "", err
	}
	if err := backupFrom(backupDir, orig, work); err != nil {
		removeSQLite(work)
		return "", err
	}
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(orig + suf); err == nil {
			_ = backupFrom(backupDir, orig+suf, orig+suf)
		}
	}
	return work, nil
}

func copySQLite(src, dst string) error {
	if err := copyFile(src, dst); err != nil {
		return err
	}
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(src + suf); err == nil {
			if err := copyFile(src+suf, dst+suf); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeSQLite(path string) {
	os.Remove(path)
	removeSQLiteSidecars(path)
}

func removeSQLiteSidecars(path string) {
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
}

func checkpointSQLite(path string) error {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	cerr := db.Close()
	if err != nil {
		return err
	}
	return cerr
}

func replaceFile(work, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Rename(work, dest); err == nil {
		return nil
	}
	// Windows cannot rename over an existing file; park the dest first.
	parked := dest + ".asm-old"
	if _, err := os.Stat(dest); err == nil {
		_ = os.Remove(parked)
		if err := os.Rename(dest, parked); err != nil {
			if err := os.Remove(dest); err != nil {
				return err
			}
		} else {
			defer os.Remove(parked)
		}
	}
	if err := os.Rename(work, dest); err == nil {
		return nil
	}
	// Last resort: copy into dest (not atomic).
	if err := copyFile(work, dest); err != nil {
		if _, err2 := os.Stat(parked); err2 == nil {
			_ = os.Rename(parked, dest)
		}
		return err
	}
	return os.Remove(work)
}

func checksum(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if !st.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		_, _ = h.Write(b)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			_, _ = h.Write([]byte("d:" + rel + "\n"))
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte("f:" + rel + "\n"))
		_, _ = h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
