package changefeed

// ResultSet is the last result a live query emitted, as canonically encoded
// rows. It is a multiset: a query may legitimately return equal rows.
type ResultSet struct {
	rows [][]byte
}

// Len returns the number of rows held.
func (rs *ResultSet) Len() int { return len(rs.rows) }

// Replace makes cur the current result and reports how it differs from the
// previous one: added lists the indexes into cur of the rows that are new, in
// result order; removed lists the previous rows that are gone, in their
// previous order. A row that merely moved is neither. ResultSet keeps cur.
func (rs *ResultSet) Replace(cur [][]byte) (added []int, removed [][]byte) {
	counts := make(map[string]int, len(rs.rows))
	for _, row := range rs.rows {
		counts[string(row)]++
	}
	for i, row := range cur {
		if counts[string(row)] > 0 {
			counts[string(row)]--
			continue
		}
		added = append(added, i)
	}
	for _, row := range rs.rows {
		if counts[string(row)] > 0 {
			counts[string(row)]--
			removed = append(removed, row)
		}
	}
	rs.rows = cur
	return added, removed
}
