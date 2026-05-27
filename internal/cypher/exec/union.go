package exec

// unionOp drains its parts in order and emits the resulting rows. With dedup=true
// (UNION without ALL), rows are de-duplicated by the projected columns using a
// canonical string key.
type unionOp struct {
	parts   []op
	columns []string
	dedup   bool

	idx  int
	seen map[string]struct{}
}

func (u *unionOp) next() (binding, bool, error) {
	for u.idx < len(u.parts) {
		b, ok, err := u.parts[u.idx].next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			u.idx++
			continue
		}
		if u.dedup {
			key := unionKey(b, u.columns)
			if _, dup := u.seen[key]; dup {
				continue
			}
			u.seen[key] = struct{}{}
		}
		return b, true, nil
	}
	return nil, false, nil
}

func unionKey(b binding, columns []string) string {
	parts := make([]any, len(columns))
	for i, c := range columns {
		parts[i] = b[c]
	}
	return canonKeyList(parts)
}
