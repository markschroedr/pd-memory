// Package rating rates how much each observation matters relative to the rest of memory.
package rating

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/markschroedr/pd-memory/internal/memory"
	"github.com/markschroedr/pd-memory/internal/model"
)

//go:embed system.txt
var system string

type rated struct {
	ID string `json:"id" jsonschema:"minLength=1"`
	memory.Ratings
}
type batch struct {
	Ratings []rated `json:"ratings"`
}

// Version identifies the rating definitions. A changed prompt or schema rates every observation again.
var Version = func() string {
	h := sha256.Sum256([]byte(system + memory.JSON(model.Schema(new(batch)))))
	return hex.EncodeToString(h[:6])
}()

const batchSize = 50

// Rate rates every current observation whose ratings are missing, describe an older line, or come
// from older definitions. Maintenance runs it before views, because ranking feeds composition. A
// failure is recorded like other maintenance failures; unrated observations rank as memory.Unrated
// until the next run.
func Rate(s *memory.Store, m *model.Client, concurrency int) error {
	err := rate(s, m, concurrency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Maintenance ratings failed: %v\n", err)
		_, err = s.DB.Exec("INSERT OR REPLACE INTO maintenance_errors VALUES('ratings',?,?)", err.Error(), time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}
	_, err = s.DB.Exec("DELETE FROM maintenance_errors WHERE id='ratings'")
	return err
}
func rate(s *memory.Store, m *model.Client, concurrency int) error {
	rows, e := s.Query("SELECT o.id FROM observations o LEFT JOIN ratings r ON r.observation=o.id AND r.line=json_extract(o.value,'$.line') AND r.version=? WHERE o.replaced_by IS NULL AND o.forgotten IS NULL AND r.observation IS NULL", Version)
	if e != nil {
		return e
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r["id"].(string))
	}
	pending, e := s.ObservationsByIDs(ids)
	if e != nil {
		return e
	}
	batches := [][]memory.Observation{}
	for start := 0; start < len(pending); start += batchSize {
		batches = append(batches, pending[start:min(start+batchSize, len(pending))])
	}
	// The first batch runs alone so later batches can calibrate against its ratings; later
	// batches run in waves, each calibrated against the waves before it.
	for start, width := 0, 1; start < len(batches); start, width = start+width, concurrency {
		wave := batches[start:min(start+width, len(batches))]
		errs := make([]error, len(wave))
		clients := make([]model.Client, len(wave))
		var wg sync.WaitGroup
		for i, b := range wave {
			clients[i] = *m
			clients[i].Cost = 0
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = rateBatch(s, &clients[i], b)
			}()
		}
		wg.Wait()
		for i := range clients {
			m.Cost += clients[i].Cost
		}
		if e = errors.Join(errs...); e != nil {
			return e
		}
	}
	return nil
}
func rateBatch(s *memory.Store, m *model.Client, obs []memory.Observation) error {
	// Calibration: the latest ratings from each reach band and the latest strong directives.
	examples, e := s.Query(`SELECT line,json(value) ratings FROM (SELECT line,value,row_number() OVER (PARTITION BY CASE WHEN json_extract(value,'$.reach')>=0.7 THEN 2 WHEN json_extract(value,'$.reach')>=0.4 THEN 1 ELSE 0 END ORDER BY rowid DESC) n FROM ratings WHERE version=?) WHERE n<=3
		UNION SELECT line,json(value) FROM (SELECT line,value FROM ratings WHERE version=? AND json_extract(value,'$.directive')>=0.6 ORDER BY rowid DESC LIMIT 2)`, Version, Version)
	if e != nil {
		return e
	}
	for _, x := range examples {
		var r map[string]any
		if e = json.Unmarshal([]byte(x["ratings"].(string)), &r); e != nil {
			return e
		}
		x["ratings"] = r
	}
	items, lines := []map[string]any{}, map[string]string{}
	for _, o := range obs {
		items = append(items, map[string]any{"id": o.ID, "line": o.Line, "body": o.Body, "happened": o.Happened, "kind": o.Kind, "claimant": o.Claimant, "authority": o.Authority, "pages": o.Pages})
		lines[o.ID] = o.Line
	}
	out, e := model.Structured[batch](m, "rating", system, memory.JSON(map[string]any{"calibration_examples": examples, "observations": items}), func(b batch) error {
		seen := map[string]bool{}
		for _, r := range b.Ratings {
			if lines[r.ID] == "" || seen[r.ID] {
				return fmt.Errorf("unknown or repeated id %s", r.ID)
			}
			seen[r.ID] = true
		}
		if len(seen) != len(lines) {
			return fmt.Errorf("rated %d of %d observations; rate every id exactly once", len(seen), len(lines))
		}
		return nil
	})
	if e != nil {
		return e
	}
	for _, r := range out.Ratings {
		if _, e = s.DB.Exec("INSERT OR REPLACE INTO ratings VALUES(?,?,?,?)", r.ID, lines[r.ID], Version, memory.JSON(r.Ratings)); e != nil {
			return e
		}
	}
	return nil
}
