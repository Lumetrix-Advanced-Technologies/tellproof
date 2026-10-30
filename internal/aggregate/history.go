package aggregate

import (
	"cmp"
	"slices"
	"time"

	"github.com/lumetrix-advanced-technologies/tellproof/internal/batch"
)

// A change (D-074) is a model, or a prompt version, that a slot starts to use:
// a model new to its slot (the agent or service, and the operation), or a
// version new to its prompt name. Models a slot uses side by side are not
// changes, however their calls interleave. What each slot has in use is the
// History, kept across windows and, saved by the agent in its data
// directory, across restarts, so a model swapped during a deploy is recorded.
//
// A value is retired once its slot has had RetireAfter windows with calls
// and none of them used it; when it comes back, that is a change again. A
// slot's first window has no history: there a value is a change only when
// every value that started before it had made its last call first (a clean
// switch), so a swap in an agent's first hour is not missed and two models
// used side by side from the start are not reported.

// RetireAfter is how many of its slot's windows with calls a value may go
// unused before it is retired.
const RetireAfter = 24

// Bounds on what History holds, so it cannot grow without limit.
const (
	maxSlots       = 1024 // per kind; the slot unused longest goes first
	maxSlotEntries = 64   // values per slot; the one unused longest goes first
)

// History is what each slot has in use: per kind ("model", "prompt"), per
// slot, the value of its latest call and, for each value in use, how many
// of the slot's windows with calls have not used it since.
type History map[string]map[string]*SlotHistory

// SlotHistory is one slot's state.
type SlotHistory struct {
	Last   string         `json:"last"`
	Seen   int64          `json:"seen"`   // the latest window with calls (Unix seconds), for the slot bound
	Unused map[string]int `json:"unused"` // value -> windows with calls since its last call
}

// NewHistory returns an empty history.
func NewHistory() History {
	return History{"model": {}, "prompt": {}}
}

// Clone returns a deep copy.
func (h History) Clone() History {
	out := NewHistory()
	for kind, slots := range h {
		if out[kind] == nil {
			out[kind] = map[string]*SlotHistory{}
		}
		for slot, sh := range slots {
			c := &SlotHistory{Last: sh.Last, Seen: sh.Seen, Unused: make(map[string]int, len(sh.Unused))}
			for v, n := range sh.Unused {
				c.Unused[v] = n
			}
			out[kind][slot] = c
		}
	}
	return out
}

// History returns a copy of what each slot has in use, to save.
func (a *Aggregator) History() History {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.history.Clone()
}

// SetHistory restores a saved history (call before the first spans).
func (a *Aggregator) SetHistory(h History) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history = NewHistory()
	for kind, slots := range h.Clone() {
		if _, ok := a.history[kind]; ok && slots != nil {
			a.history[kind] = slots
		}
	}
}

// changes finds the window's changes and updates the history.
func (a *Aggregator) changes(w *open) []batch.Transition {
	type valueUse struct {
		value string
		u     *use
	}
	groups := map[useKey][]valueUse{} // value 0: the slot
	for k, u := range w.uses {
		g := useKey{kind: k.kind, slot: k.slot}
		groups[g] = append(groups[g], valueUse{w.strs[k.value], u})
	}
	keys := make([]useKey, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	slices.SortFunc(keys, func(x, y useKey) int {
		return cmp.Or(cmp.Compare(x.kind, y.kind), cmp.Compare(w.strs[x.slot], w.strs[y.slot]))
	})
	out := []batch.Transition{}
	for _, g := range keys {
		vals := groups[g]
		slices.SortFunc(vals, func(x, y valueUse) int { return w.compare(x.u.first, y.u.first) })
		slot := w.strs[g.slot]
		slots := a.history[g.kind]
		sh := slots[slot]
		at := func(v valueUse) string { return iso(time.Unix(0, v.u.first.start)) }
		if sh == nil {
			// No history: a value is a change only after a clean switch.
			for i, v := range vals {
				var prev *valueUse
				clean := i > 0
				for j := range vals[:i] {
					if w.compare(vals[j].u.last, v.u.first) > 0 {
						clean = false
						break
					}
					if prev == nil || w.compare(vals[j].u.last, prev.u.last) > 0 {
						prev = &vals[j]
					}
				}
				if clean {
					out = append(out, batch.Transition{At: at(v), Kind: g.kind, From: prev.value, To: v.value})
				}
			}
			sh = &SlotHistory{Unused: map[string]int{}}
			slots[slot] = sh
		} else {
			for _, v := range vals {
				if _, inUse := sh.Unused[v.value]; !inUse && v.value != sh.Last {
					out = append(out, batch.Transition{At: at(v), Kind: g.kind, From: sh.Last, To: v.value})
				}
			}
		}
		// Update: the values used now are in use; the others age, and retire.
		used := map[string]bool{}
		latest := vals[0]
		for _, v := range vals {
			used[v.value] = true
			if w.compare(v.u.last, latest.u.last) > 0 {
				latest = v
			}
		}
		for v := range sh.Unused {
			if !used[v] {
				sh.Unused[v]++
				if sh.Unused[v] >= RetireAfter {
					delete(sh.Unused, v)
				}
			}
		}
		for v := range used {
			sh.Unused[v] = 0
		}
		sh.Last = latest.value
		sh.Seen = time.Unix(0, latest.u.last.start).Unix()
		trimValues(sh)
	}
	for _, slots := range a.history {
		trimSlots(slots)
	}
	slices.SortFunc(out, func(x, y batch.Transition) int {
		return cmp.Or(cmp.Compare(x.At, y.At), cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.From, y.From), cmp.Compare(x.To, y.To))
	})
	return out
}

// trimValues keeps a slot's values within maxSlotEntries, dropping those
// unused longest (never the latest).
func trimValues(sh *SlotHistory) {
	for len(sh.Unused) > maxSlotEntries {
		var drop string
		n := -1
		for v, u := range sh.Unused {
			if v != sh.Last && (u > n || (u == n && v > drop)) {
				drop, n = v, u
			}
		}
		delete(sh.Unused, drop)
	}
}

// trimSlots keeps a kind's slots within maxSlots, dropping the slot unused longest.
func trimSlots(slots map[string]*SlotHistory) {
	for len(slots) > maxSlots {
		var drop string
		var seen int64
		first := true
		for s, sh := range slots {
			if first || sh.Seen < seen || (sh.Seen == seen && s > drop) {
				drop, seen, first = s, sh.Seen, false
			}
		}
		delete(slots, drop)
	}
}
