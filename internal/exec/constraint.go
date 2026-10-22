package exec

import (
	"fmt"
	"sort"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
)

// --- CREATE / DROP / SHOW CONSTRAINT ---

type createConstraintOp struct {
	ctx   *Context
	name  string
	label string
	prop  string
	done  bool
}

func (c *createConstraintOp) next() (binding, bool, error) {
	if c.done {
		return nil, false, nil
	}
	c.done = true
	txn := c.ctx.Txn
	labelID, err := catalog.InternLabel(txn, c.label)
	if err != nil {
		return nil, false, err
	}
	keyID, err := catalog.InternKey(txn, c.prop)
	if err != nil {
		return nil, false, err
	}
	if def, found, err := catalog.GetConstraint(txn, labelID, keyID); err != nil {
		return nil, false, err
	} else if found {
		// Idempotent, unless it asks for a different name.
		_, _, existing, err := catalog.ConstraintName(txn, def)
		if err != nil {
			return nil, false, err
		}
		if c.name != "" && c.name != existing {
			return nil, false, fmt.Errorf("exec: a constraint on :%s(%s) already exists as %q", c.label, c.prop, existing)
		}
		return binding{}, true, nil
	}
	if c.name != "" {
		if _, found, err := findConstraint(c.ctx, c.name); err != nil {
			return nil, false, err
		} else if found {
			return nil, false, fmt.Errorf("exec: a constraint named %q already exists", c.name)
		}
	}
	if err := graph.AddUniqueConstraint(txn, labelID, keyID, c.name); err != nil {
		return nil, false, err
	}
	return binding{}, true, nil
}

// findConstraint returns the constraint known by name.
func findConstraint(ctx *Context, name string) (catalog.ConstraintDef, bool, error) {
	defs, err := catalog.ListConstraints(ctx.Txn)
	if err != nil {
		return catalog.ConstraintDef{}, false, err
	}
	for _, d := range defs {
		_, _, n, err := catalog.ConstraintName(ctx.Txn, d)
		if err != nil {
			return catalog.ConstraintDef{}, false, err
		}
		if n == name {
			return d, true, nil
		}
	}
	return catalog.ConstraintDef{}, false, nil
}

type dropConstraintOp struct {
	ctx      *Context
	name     string
	ifExists bool
	done     bool
}

func (d *dropConstraintOp) next() (binding, bool, error) {
	if d.done {
		return nil, false, nil
	}
	d.done = true
	def, found, err := findConstraint(d.ctx, d.name)
	if err != nil {
		return nil, false, err
	}
	if !found {
		if d.ifExists {
			return binding{}, true, nil
		}
		return nil, false, fmt.Errorf("exec: no constraint named %q", d.name)
	}
	if err := graph.DropUniqueConstraint(d.ctx.Txn, def.Label, def.PropKey); err != nil {
		return nil, false, err
	}
	return binding{}, true, nil
}

// showConstraintsOp emits one (name, label, property, type) row per
// constraint, by name.
type showConstraintsOp struct {
	ctx  *Context
	rows []binding
	pos  int
	done bool
}

func (s *showConstraintsOp) next() (binding, bool, error) {
	if !s.done {
		s.done = true
		defs, err := catalog.ListConstraints(s.ctx.Txn)
		if err != nil {
			return nil, false, err
		}
		for _, d := range defs {
			label, prop, name, err := catalog.ConstraintName(s.ctx.Txn, d)
			if err != nil {
				return nil, false, err
			}
			s.rows = append(s.rows, binding{"name": name, "label": label, "property": prop, "type": "UNIQUE"})
		}
		sort.Slice(s.rows, func(i, j int) bool { return s.rows[i]["name"].(string) < s.rows[j]["name"].(string) })
	}
	if s.pos >= len(s.rows) {
		return nil, false, nil
	}
	r := s.rows[s.pos]
	s.pos++
	return r, true, nil
}
