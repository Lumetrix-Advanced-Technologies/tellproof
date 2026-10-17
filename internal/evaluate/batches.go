package evaluate

import (
	"fmt"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
)

// FromBatches turns batches, in the order they were sent, into the
// evaluation's windows, billed usage and evaluation runs: what the pack
// issuer and `tellproof-agent check` both evaluate, so the two cannot drift.
// Provider usage reported for the same provider, model, region and hour by
// more than one batch (the early first batch and the hour's own) counts
// once, the larger report winning; rows keep their first-seen order.
func FromBatches(bs []batch.Batch) (windows []Window, usage []UsageRow, evals []batch.Eval, err error) {
	type key struct {
		provider, model, region string
		hour                    time.Time
	}
	rows := map[key]UsageRow{}
	var order []key
	for _, b := range bs {
		hour, err := time.Parse(time.RFC3339, b.Window.Start)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("evaluate: batch %d: window start %q: %w", b.Seq, b.Window.Start, err)
		}
		windows = append(windows, Window{Window: b.Window, Metrics: b.Metrics, Observed: b.Observed, Commit: b.Commit})
		evals = append(evals, b.Metrics.Evals...)
		for _, u := range b.Usage {
			k := key{u.Provider, u.Model, u.Region, hour.UTC()}
			row := UsageRow{Provider: u.Provider, Model: u.Model, Region: u.Region, Start: hour.UTC(),
				InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, Requests: u.Requests}
			old, seen := rows[k]
			if !seen {
				order = append(order, k)
			}
			if !seen || row.InputTokens+row.OutputTokens > old.InputTokens+old.OutputTokens {
				rows[k] = row
			}
		}
	}
	for _, k := range order {
		usage = append(usage, rows[k])
	}
	return windows, usage, evals, nil
}
