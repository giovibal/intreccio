package changefeed

import (
	"reflect"
	"sort"
	"testing"

	"github.com/giovibal/intreccio/query/parser"
)

func sorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestFootprintOf(t *testing.T) {
	type want struct {
		labels, types, keys       []string
		anyLabel, anyType, anyKey bool
	}
	cases := []struct {
		src  string
		want want
	}{
		{"MATCH (n:Person) RETURN n.name",
			want{labels: []string{"Person"}, keys: []string{"name"}}},
		{"MATCH (n:Person) RETURN n",
			want{labels: []string{"Person"}, anyKey: true}},
		{"MATCH (n) RETURN n.name",
			want{anyLabel: true, keys: []string{"name"}}},
		{"MATCH (a:Person)-[:KNOWS]->(b:Company) RETURN a.name, b.size",
			want{labels: []string{"Company", "Person"}, types: []string{"KNOWS"}, keys: []string{"name", "size"}}},
		{"MATCH (a:Person)-[r]->(b) RETURN type(r), id(b), labels(a), count(a)",
			want{labels: []string{"Person"}, anyLabel: true, anyType: true}},
		{"MATCH (a:Person {email: $e}) WHERE a.age > 18 AND a.nick IS NULL RETURN count(*) AS c",
			want{labels: []string{"Person"}, keys: []string{"age", "email", "nick"}}},
		{"MATCH (a:P) WITH a AS x RETURN x.name",
			want{labels: []string{"P"}, keys: []string{"name"}, anyKey: true}},
		{"MATCH (a:P) WITH a.name AS name WHERE name STARTS WITH 'a' RETURN name",
			want{labels: []string{"P"}, keys: []string{"name"}}},
		{"MATCH (a:P) RETURN *",
			want{labels: []string{"P"}, anyKey: true}},
		{"MATCH (a:P) WHERE a:Q RETURN id(a)",
			want{labels: []string{"P"}}},
		{"MATCH (a:P)-[:T1|T2*1..3]->(b:Q) RETURN b.x ORDER BY b.y SKIP 1 LIMIT 2",
			want{labels: []string{"P", "Q"}, types: []string{"T1", "T2"}, keys: []string{"x", "y"}}},
		{"MATCH (a:P)-[r:T {w: 1}]->(b:Q) RETURN r",
			want{labels: []string{"P", "Q"}, types: []string{"T"}, anyKey: true}},
		{"MATCH (a:P) RETURN a.x AS v UNION MATCH (b:Q)-[:T]->() RETURN b.y AS v",
			want{labels: []string{"P", "Q"}, types: []string{"T"}, keys: []string{"x", "y"}, anyLabel: true}},
		{"MATCH (a:P) OPTIONAL MATCH (a)-[:T]->(b:Q) RETURN a.x, b.y",
			want{labels: []string{"P", "Q"}, types: []string{"T"}, keys: []string{"x", "y"}, anyLabel: true}},
		{"MATCH (a:P) RETURN properties(a)",
			want{labels: []string{"P"}, anyKey: true}},
		{"MATCH (a:P) RETURN collect(a.name) AS names, [a.x, {k: a.y}] AS l, CASE WHEN a.z > 1 THEN a.w ELSE 0 END AS c",
			want{labels: []string{"P"}, keys: []string{"name", "w", "x", "y", "z"}}},
		{"MATCH (a:P) RETURN collect(a)",
			want{labels: []string{"P"}, anyKey: true}},
		{"MATCH (a:P) UNWIND a.tags AS tag RETURN tag",
			want{labels: []string{"P"}, keys: []string{"tags"}}},
		{"MATCH (a:P:Q) RETURN count(DISTINCT a)",
			want{labels: []string{"P", "Q"}}},
		{"RETURN 1 AS one", want{}},
		{"UNWIND $l AS x RETURN x", want{}},
		// A writing query is never watched; if one gets here, assume everything.
		{"MATCH (a:P) SET a.x = 1 RETURN a.x",
			want{anyLabel: true, anyType: true, anyKey: true}},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			q, err := parser.Parse(c.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			fp := FootprintOf(q)
			if c.want.anyLabel && c.want.anyType && c.want.anyKey {
				if !fp.AnyLabel || !fp.AnyType || !fp.AnyKey {
					t.Fatalf("footprint = %+v, want everything", fp)
				}
				return
			}
			got := want{
				labels: sorted(fp.Labels), types: sorted(fp.Types), keys: sorted(fp.Keys),
				anyLabel: fp.AnyLabel, anyType: fp.AnyType, anyKey: fp.AnyKey,
			}
			w := c.want
			if w.labels == nil {
				w.labels = []string{}
			}
			if w.types == nil {
				w.types = []string{}
			}
			if w.keys == nil {
				w.keys = []string{}
			}
			if !reflect.DeepEqual(got, w) {
				t.Errorf("footprint = %+v, want %+v", got, w)
			}
		})
	}
}
