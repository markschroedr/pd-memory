package fold

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/markschroedr/pd-memory/internal/config"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
	"github.com/markschroedr/pd-memory/internal/view"
)

type Worker struct {
	Config *config.Config
	Log    *inputlog.Store
	Memory *memory.Store
	Model  *model.Client
	Seq    int64
	out    *Outcome
}
type Outcome struct {
	Processed   []int64     `json:"processed"`
	Failed      []int64     `json:"failed"`
	Maintenance view.Result `json:"maintenance"`
	Cost        float64     `json:"cost_usd"`
	Busy        bool        `json:"busy,omitempty"`
}

func Lock(dir string, wait bool) (*os.File, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	flags := syscall.LOCK_EX
	if !wait {
		flags |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(f.Fd()), flags); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func Unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }

// Manual lock holders must hand off writes admitted while they held the lock.
// Recorded failures remain failed until a retry; maintenance alone never wakes again.
func Release(c *config.Config, lock *os.File) error {
	Unlock(lock)
	s, e := memory.Open(c.Workspace.Dir, false)
	if e != nil {
		return e
	}
	var pending bool
	e = s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM inputlog.entries l LEFT JOIN fold_state f ON f.seq=l.seq WHERE f.seq IS NULL OR (f.status='failed' AND f.error IS NULL))").Scan(&pending)
	s.Close()
	if e != nil {
		return e
	}
	if pending {
		return Wake(c)
	}
	return nil
}
func Wake(c *config.Config) error {
	lock, e := Lock(c.Workspace.Dir, false)
	if errors.Is(e, syscall.EWOULDBLOCK) {
		return nil
	}
	if e != nil {
		return e
	}
	Unlock(lock)
	binary, e := os.Executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(binary, "worker", "--config", c.Path, "--json")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	null, e := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if e != nil {
		return e
	}
	defer null.Close()
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	if e = cmd.Start(); e != nil {
		return e
	}
	return cmd.Process.Release()
}
func New(c *config.Config) (*Worker, error) {
	l, e := inputlog.Open(c.Workspace.Dir, true)
	if e != nil {
		return nil, e
	}
	s, e := memory.Open(c.Workspace.Dir, true)
	if e != nil {
		l.Close()
		return nil, e
	}
	w := &Worker{Config: c, Log: l, Memory: s}
	w.Model = &model.Client{Config: c, Record: func(call model.Call) error {
		var seq any
		if w.Seq > 0 {
			seq = w.Seq
		}
		return s.Tx(func(conn *sql.Conn) error {
			return memory.Exec(conn, "INSERT INTO model_calls(seq,phase,request_id,model,tier,usage,cost_usd,repaired,error,created) VALUES(?,?,?,?,?,?,?,?,?,?)", seq, call.Phase, call.RequestID, call.Model, call.Tier, memory.JSON(call.Usage), call.Cost, call.Repaired, call.Error, time.Now().UTC().Format(time.RFC3339Nano))
		})
	}, Invalid: func(err error) error {
		return s.Tx(func(conn *sql.Conn) error {
			return memory.Exec(conn, "UPDATE model_calls SET phase=phase||'_invalid',error=? WHERE id=(SELECT max(id) FROM model_calls)", err.Error())
		})
	}}
	return w, nil
}
func (w *Worker) Close() { w.Memory.Close(); w.Log.Close() }
func (w *Worker) done(c *sql.Conn, seq int64, message string) error {
	var note any
	if message != "" {
		note = message
	}
	return memory.Exec(c, "INSERT INTO fold_state VALUES(?,'done',1,?) ON CONFLICT(seq) DO UPDATE SET status='done',error=excluded.error", seq, note)
}
func (w *Worker) eligible(e inputlog.Entry) (bool, error) {
	var status string
	var last *string
	err := w.Memory.DB.QueryRow("SELECT status,error FROM fold_state WHERE seq=?", e.Seq).Scan(&status, &last)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return status != "done" && last == nil, nil
}
func (w *Worker) retry(seq *int64, eligibleOnly bool) error {
	return w.Memory.Tx(func(c *sql.Conn) error {
		q := "UPDATE fold_state SET error=NULL WHERE status='failed'"
		args := []any{}
		if seq != nil {
			q += " AND seq=?"
			args = append(args, *seq)
		}
		if eligibleOnly {
			q += " AND attempts<3"
		}
		return memory.Exec(c, q, args...)
	})
}
func (w *Worker) entry(e inputlog.Entry) error {
	switch e.Kind {
	case "source":
		var src inputlog.Source
		if err := model.Decode(e.Payload, &src); err != nil {
			return err
		}
		return w.source(e, src)
	case "note", "edit", "forget", "focus":
		return w.structured(e)
	}
	return fmt.Errorf("unknown entry kind %s", e.Kind)
}
func (w *Worker) drain(out *Outcome) error {
	w.out = out
	for {
		entries, e := w.Log.Entries()
		if e != nil {
			return e
		}
		var next *inputlog.Entry
		for i := range entries {
			entry := entries[i]
			ok, e := w.eligible(entry)
			if e != nil {
				return e
			}
			if !ok {
				continue
			}
			if next == nil {
				copy := entry
				next = &copy
			}
			if entry.Kind != "source" {
				resolved, e := w.referencesResolve(entry)
				if e != nil {
					return e
				}
				if resolved {
					copy := entry
					next = &copy
					break
				}
			}
		}
		if next == nil {
			return nil
		}
		if e = w.foldOne(*next); e != nil {
			return e
		}
	}
}
func (w *Worker) foldOne(entry inputlog.Entry) error {
	previous := w.Seq
	w.Seq = entry.Seq
	defer func() { w.Seq = previous }()
	if e := w.Memory.Tx(func(c *sql.Conn) error {
		return memory.Exec(c, "INSERT INTO fold_state VALUES(?,'failed',1,NULL) ON CONFLICT(seq) DO UPDATE SET attempts=attempts+1,error=NULL", entry.Seq)
	}); e != nil {
		return e
	}
	if e := w.entry(entry); e != nil {
		if err := w.Memory.Tx(func(c *sql.Conn) error {
			return memory.Exec(c, "UPDATE fold_state SET error=? WHERE seq=?", e.Error(), entry.Seq)
		}); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Fold %d failed: %v\n", entry.Seq, e)
		w.out.Failed = append(w.out.Failed, entry.Seq)
	} else {
		w.out.Processed = append(w.out.Processed, entry.Seq)
	}
	return nil
}
func (w *Worker) pendingStructured() error {
	for {
		entries, e := w.Log.Entries()
		if e != nil {
			return e
		}
		found := false
		for _, entry := range entries {
			if entry.Kind == "source" {
				continue
			}
			eligible, e := w.eligible(entry)
			if e != nil {
				return e
			}
			if eligible {
				resolved, e := w.referencesResolve(entry)
				if e != nil {
					return e
				}
				if resolved {
					if e = w.foldOne(entry); e != nil {
						return e
					}
					found = true
					break
				}
			}
		}
		if !found {
			return nil
		}
	}
}
func Run(c *config.Config, wait, retry bool) (Outcome, error) {
	out := Outcome{Processed: []int64{}, Failed: []int64{}, Maintenance: view.Result{Built: []string{}, Failed: []view.Failure{}}}
	for {
		lock, e := Lock(c.Workspace.Dir, wait)
		if errors.Is(e, syscall.EWOULDBLOCK) {
			out.Busy = true
			return out, nil
		}
		if e != nil {
			return out, e
		}
		w, e := New(c)
		if e != nil {
			Unlock(lock)
			return out, e
		}
		if retry {
			e = w.retry(nil, true)
			retry = false
		}
		if e == nil {
			e = w.drain(&out)
		}
		var entries []inputlog.Entry
		if e == nil {
			entries, e = w.Log.Entries()
		}
		seen := int64(0)
		if len(entries) > 0 {
			seen = entries[len(entries)-1].Seq
		}
		w.Seq = 0
		if e == nil {
			out.Maintenance, e = (&view.Engine{Store: w.Memory, Config: c, Model: w.Model}).Maintain(entries)
		}
		out.Cost += w.Model.Cost
		w.Close()
		Unlock(lock)
		if e != nil {
			return out, e
		}
		l, e := inputlog.Open(c.Workspace.Dir, false)
		if e != nil {
			return out, e
		}
		var last int64
		e = l.DB.QueryRow("SELECT coalesce(max(seq),0) FROM entries").Scan(&last)
		l.Close()
		if e != nil {
			return out, e
		}
		if last <= seen {
			return out, nil
		}
		wait = false
	}
}
func Wait(c *config.Config, seqs []int64, retry bool) (Outcome, error) {
	out, e := Run(c, false, retry)
	if e != nil {
		return out, e
	}
	if !out.Busy {
		return out, completion(c, seqs)
	}
	for {
		time.Sleep(100 * time.Millisecond)
		lock, err := Lock(c.Workspace.Dir, false)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			continue
		}
		if err != nil {
			return out, err
		}
		Unlock(lock)
		s, err := memory.Open(c.Workspace.Dir, false)
		if err != nil {
			return out, err
		}
		pending := false
		for _, seq := range seqs {
			var status string
			var errorText *string
			err = s.DB.QueryRow("SELECT status,error FROM fold_state WHERE seq=?", seq).Scan(&status, &errorText)
			if err != nil && err != sql.ErrNoRows {
				s.Close()
				return out, err
			}
			if err == sql.ErrNoRows || (status == "failed" && errorText == nil) {
				pending = true
			}
		}
		s.Close()
		if pending {
			return Wait(c, seqs, retry)
		}
		out.Busy = false
		return out, completion(c, seqs)
	}
}
func completion(c *config.Config, seqs []int64) error {
	s, e := memory.Open(c.Workspace.Dir, false)
	if e != nil {
		return e
	}
	defer s.Close()
	for _, seq := range seqs {
		var status string
		var message *string
		if e = s.DB.QueryRow("SELECT status,error FROM fold_state WHERE seq=?", seq).Scan(&status, &message); e != nil {
			return e
		}
		if status != "done" {
			msg := "unfinished"
			if message != nil {
				msg = *message
			}
			if strings.HasPrefix(msg, "edit conflict:") {
				return &inputlog.Conflict{Message: fmt.Sprintf("fold %d failed: %s", seq, msg)}
			}
			return fmt.Errorf("fold %d failed: %s", seq, msg)
		}
	}
	return nil
}
func Maintenance(c *config.Config, folder string, budget int, compose bool) (out view.Result, err error) {
	lock, e := Lock(c.Workspace.Dir, true)
	if e != nil {
		return view.Result{}, e
	}
	defer func() { err = errors.Join(err, Release(c, lock)) }()
	w, e := New(c)
	if e != nil {
		return view.Result{}, e
	}
	defer w.Close()
	engine := &view.Engine{Store: w.Memory, Config: c, Model: w.Model}
	if compose {
		if budget == 0 {
			budget = c.Brief.Budget
			if folder != "" {
				budget = 4000
			}
		}
		v, e := engine.Composition(folder, budget)
		return view.Result{Built: []string{v.ID}, Failed: []view.Failure{}, Cost: w.Model.Cost}, e
	}
	entries, e := w.Log.Entries()
	if e != nil {
		return view.Result{}, e
	}
	return engine.Maintain(entries)
}
func Retry(c *config.Config, seq *int64) (Outcome, error) {
	lock, e := Lock(c.Workspace.Dir, true)
	if e != nil {
		return Outcome{}, e
	}
	w, e := New(c)
	if e == nil {
		e = w.retry(seq, false)
		w.Close()
	}
	Unlock(lock)
	if e != nil {
		return Outcome{}, e
	}
	return Run(c, true, false)
}
func Reindex(c *config.Config) (out Outcome, err error) {
	lock, e := Lock(c.Workspace.Dir, true)
	if e != nil {
		return Outcome{}, e
	}
	defer func() { err = errors.Join(err, Release(c, lock)) }()
	w, e := New(c)
	if e != nil {
		return Outcome{}, e
	}
	defer w.Close()
	obs, e := w.Memory.Observations(true)
	if e != nil {
		return Outcome{}, e
	}
	inputs, ids := []string{}, []string{}
	for _, o := range obs {
		inputs = append(inputs, o.Text())
		ids = append(ids, o.ID)
	}
	entries, e := w.Log.Entries()
	if e != nil {
		return Outcome{}, e
	}
	for _, entry := range entries {
		chunks, e := w.Memory.Chunks(entry.Seq)
		if e != nil {
			return Outcome{}, e
		}
		for _, ch := range chunks {
			inputs = append(inputs, ch.Context+"\n"+ch.Text)
			ids = append(ids, ch.ID)
		}
	}
	vectors := [][]float64{}
	for start := 0; start < len(inputs); start += 64 {
		end := min(len(inputs), start+64)
		v, e := w.Model.Embed(inputs[start:end])
		if e != nil {
			return Outcome{}, e
		}
		vectors = append(vectors, v...)
	}
	e = w.Memory.Tx(func(conn *sql.Conn) error {
		if e := memory.Exec(conn, "DELETE FROM embeddings; DELETE FROM observations_fts; DELETE FROM chunks_fts; INSERT INTO observations_fts SELECT id,json_extract(value,'$.line'),coalesce(json_extract(value,'$.body'),'') FROM observations"); e != nil {
			return e
		}
		for i, id := range ids {
			if e := putVector(conn, id, c, vectors[i]); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return Outcome{}, e
	}
	for _, entry := range entries {
		chunks, e := w.Memory.Chunks(entry.Seq)
		if e != nil {
			return Outcome{}, e
		}
		e = w.Memory.Tx(func(conn *sql.Conn) error {
			for _, ch := range chunks {
				if e := memory.Exec(conn, "INSERT INTO chunks_fts VALUES(?,?,?)", ch.ID, ch.Context, ch.Text); e != nil {
					return e
				}
			}
			return nil
		})
		if e != nil {
			return Outcome{}, e
		}
	}
	return Outcome{Processed: []int64{}, Failed: []int64{}, Maintenance: view.Result{Built: []string{}, Failed: []view.Failure{}}, Cost: w.Model.Cost}, nil
}
func Rebuild(c *config.Config) (out Outcome, err error) {
	lock, e := Lock(c.Workspace.Dir, true)
	if e != nil {
		return Outcome{}, e
	}
	defer func() { err = errors.Join(err, Release(c, lock)) }()
	path := filepath.Join(c.Workspace.Dir, "memory.db")
	backup := path + ".before-rebuild-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, e = os.Stat(path + suffix); e == nil {
			if e = os.Rename(path+suffix, backup+suffix); e != nil {
				return Outcome{}, e
			}
		} else if !os.IsNotExist(e) {
			return Outcome{}, e
		}
	}
	w, e := New(c)
	if e != nil {
		return Outcome{}, e
	}
	defer w.Close()
	out = Outcome{Processed: []int64{}, Failed: []int64{}}
	if e = w.drain(&out); e != nil {
		return out, e
	}
	entries, e := w.Log.Entries()
	if e != nil {
		return out, e
	}
	w.Seq = 0
	out.Maintenance, e = (&view.Engine{Store: w.Memory, Config: c, Model: w.Model}).Maintain(entries)
	out.Cost = w.Model.Cost
	return out, e
}
func DecodePayload[T any](raw json.RawMessage) (T, error) {
	var t T
	e := model.Decode(raw, &t)
	return t, e
}
