package fold

import (
	"encoding/json"
	"fmt"
	inputlog "github.com/markschroedr/pd-memory/internal/log"
)

// Digests are context, never evidence. Leave earlier chunk ids out of this prompt.
func (w *Worker) incrementContext(e inputlog.Entry, src inputlog.Source) (string, error) {
	if src.Session == nil || *src.Session == "" {
		return "", nil
	}
	rows, err := w.Memory.DB.Query(`SELECT json_extract(l.payload,'$.label'),coalesce(json_extract(l.payload,'$.happened'),l.created),json_extract(x.value,'$.source_digest')
 FROM inputlog.entries l JOIN extraction x ON x.seq=l.seq
 WHERE l.kind='source' AND l.seq<? AND json_extract(l.payload,'$.session')=?
 ORDER BY coalesce(json_extract(l.payload,'$.happened'),l.created),l.seq`, e.Seq, *src.Session)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type digest struct {
		Label    string `json:"label"`
		Happened string `json:"happened"`
		Digest   string `json:"digest"`
	}
	prior := []digest{}
	for rows.Next() {
		var d digest
		if err = rows.Scan(&d.Label, &d.Happened, &d.Digest); err != nil {
			return "", err
		}
		prior = append(prior, d)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if len(prior) == 0 {
		return "", nil
	}
	b, err := json.Marshal(prior)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\n\n<earlier_unit_digests_read_only>\n%s\n</earlier_unit_digests_read_only>", b), nil
}
