package changefeed

import "sort"

// Mirror is a copy of a live query's result kept by whoever consumes it from
// afar: the multiset of its canonically encoded rows. It follows the result by
// applying the batches it receives, and it is what lets a consumer change
// source — after losing the member that was serving it — and learn only the
// net difference between what it already had and the new source's result.
type Mirror struct {
	counts map[string]int
}

// Len returns the number of rows held.
func (m *Mirror) Len() int {
	n := 0
	for _, c := range m.counts {
		n += c
	}
	return n
}

// Apply folds one batch into the mirror.
func (m *Mirror) Apply(added, removed [][]byte) {
	if m.counts == nil {
		m.counts = map[string]int{}
	}
	for _, row := range removed {
		k := string(row)
		if m.counts[k] <= 1 {
			delete(m.counts, k)
		} else {
			m.counts[k]--
		}
	}
	for _, row := range added {
		m.counts[string(row)]++
	}
}

// Reset makes snapshot the mirrored result and reports how it differs from what
// the mirror held: added lists the indexes into snapshot of the rows that are
// new, in snapshot order; removed lists the rows that are gone, in a
// deterministic (byte) order.
func (m *Mirror) Reset(snapshot [][]byte) (added []int, removed [][]byte) {
	left := m.counts
	next := make(map[string]int, len(snapshot))
	for i, row := range snapshot {
		k := string(row)
		next[k]++
		if left[k] > 0 {
			left[k]--
			continue
		}
		added = append(added, i)
	}
	gone := make([]string, 0)
	for k, c := range left {
		for range c {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	for _, k := range gone {
		removed = append(removed, []byte(k))
	}
	m.counts = next
	return added, removed
}
