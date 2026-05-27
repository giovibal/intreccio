package parser

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// --- AST constructor helpers ---

func lit(v any) *ast.Literal                      { return &ast.Literal{Value: v} }
func vr(n string) *ast.Variable                   { return &ast.Variable{Name: n} }
func par(n string) *ast.Param                     { return &ast.Param{Name: n} }
func pa(t ast.Expr, k string) *ast.PropertyAccess { return &ast.PropertyAccess{Target: t, Key: k} }
func bin(op string, l, r ast.Expr) *ast.Binary    { return &ast.Binary{Op: op, Left: l, Right: r} }
func un(op string, e ast.Expr) *ast.Unary         { return &ast.Unary{Op: op, Expr: e} }

func node(v string, labels []string, props map[string]ast.Expr) *ast.NodePattern {
	return &ast.NodePattern{Variable: v, Labels: labels, Props: props}
}
func rel(v string, types []string, dir ast.Direction) *ast.RelPattern {
	return &ast.RelPattern{Variable: v, Types: types, Direction: dir, MinHops: -1, MaxHops: -1}
}
func part(start *ast.NodePattern, chain ...ast.PatternChain) ast.PatternPart {
	return ast.PatternPart{Start: start, Chain: chain}
}

func TestParseValidCorpus(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want *ast.Query
	}{
		{
			"match-return",
			"MATCH (n:Person) RETURN n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("n", []string{"Person"}, nil))}},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("n")}}},
			}},
		},
		{
			"where-and-comparison",
			"MATCH (p:Person) WHERE p.age > 30 AND p.name = 'Alice' RETURN p",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{
					Parts: []ast.PatternPart{part(node("p", []string{"Person"}, nil))},
					Where: bin("AND",
						bin(">", pa(vr("p"), "age"), lit(int64(30))),
						bin("=", pa(vr("p"), "name"), lit("Alice"))),
				},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("p")}}},
			}},
		},
		{
			"create-node-with-props",
			"CREATE (n:Person {name: 'Bob', age: 42})",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Create{Parts: []ast.PatternPart{part(node("n", []string{"Person"},
					map[string]ast.Expr{"name": lit("Bob"), "age": lit(int64(42))}))}},
			}},
		},
		{
			"set-arithmetic",
			"MATCH (n:Person) SET n.age = n.age + 1",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("n", []string{"Person"}, nil))}},
				&ast.Set{Items: []ast.SetItem{{
					Target: pa(vr("n"), "age"),
					Value:  bin("+", pa(vr("n"), "age"), lit(int64(1))),
				}}},
			}},
		},
		{
			"detach-delete",
			"MATCH (n) DETACH DELETE n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("n", nil, nil))}},
				&ast.Delete{Detach: true, Exprs: []ast.Expr{vr("n")}},
			}},
		},
		{
			"var-length-outgoing",
			"MATCH (a)-[:KNOWS*1..3]->(b) RETURN b",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("a", nil, nil),
					ast.PatternChain{
						Rel:  &ast.RelPattern{Types: []string{"KNOWS"}, Direction: ast.DirOut, VarLength: true, MinHops: 1, MaxHops: 3},
						Node: node("b", nil, nil),
					})}},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("b")}}},
			}},
		},
		{
			"incoming-rel-with-var",
			"MATCH (a)<-[r:LIKES]-(b) RETURN r",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("a", nil, nil),
					ast.PatternChain{Rel: rel("r", []string{"LIKES"}, ast.DirIn), Node: node("b", nil, nil)})}},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("r")}}},
			}},
		},
		{
			"create-index",
			"CREATE INDEX FOR (p:Person) ON (p.email)",
			&ast.Query{Clauses: []ast.Clause{
				&ast.CreateIndex{Variable: "p", Label: "Person", Property: "email"},
			}},
		},
		{
			"merge",
			"MERGE (n:Person {email: $e})",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Merge{Part: part(node("n", []string{"Person"}, map[string]ast.Expr{"email": par("e")}))},
			}},
		},
		{
			"return-distinct",
			"RETURN DISTINCT a, b",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Return{Distinct: true, Items: []ast.ReturnItem{{Expr: vr("a")}, {Expr: vr("b")}}},
			}},
		},
		{
			"aggregation-count-distinct",
			"MATCH (p) RETURN count(DISTINCT p) AS c",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("p", nil, nil))}},
				&ast.Return{Items: []ast.ReturnItem{{
					Expr:  &ast.FunctionCall{Name: "count", Distinct: true, Args: []ast.Expr{vr("p")}},
					Alias: "c",
				}}},
			}},
		},
		{
			"not-precedence",
			"MATCH (n) WHERE NOT n.x = 1 RETURN n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{
					Parts: []ast.PatternPart{part(node("n", nil, nil))},
					Where: un("NOT", bin("=", pa(vr("n"), "x"), lit(int64(1)))),
				},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("n")}}},
			}},
		},
		{
			"is-null",
			"MATCH (n) WHERE n.email IS NULL RETURN n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{
					Parts: []ast.PatternPart{part(node("n", nil, nil))},
					Where: un("IS NULL", pa(vr("n"), "email")),
				},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("n")}}},
			}},
		},
		{
			"is-not-null",
			"MATCH (n) WHERE n.email IS NOT NULL RETURN n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{
					Parts: []ast.PatternPart{part(node("n", nil, nil))},
					Where: un("IS NOT NULL", pa(vr("n"), "email")),
				},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("n")}}},
			}},
		},
		{
			"starts-with",
			"MATCH (n) WHERE n.name STARTS WITH 'A' RETURN n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{
					Parts: []ast.PatternPart{part(node("n", nil, nil))},
					Where: bin("STARTS WITH", pa(vr("n"), "name"), lit("A")),
				},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("n")}}},
			}},
		},
		{
			"in-list-literal",
			"MATCH (n) WHERE n.country IN ['IT', 'DE'] RETURN n",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{
					Parts: []ast.PatternPart{part(node("n", nil, nil))},
					Where: bin("IN", pa(vr("n"), "country"), &ast.ListLiteral{Elements: []ast.Expr{lit("IT"), lit("DE")}}),
				},
				&ast.Return{Items: []ast.ReturnItem{{Expr: vr("n")}}},
			}},
		},
		{
			"case-searched",
			"RETURN CASE WHEN 1 = 1 THEN 'yes' ELSE 'no' END AS r",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Return{Items: []ast.ReturnItem{{
					Expr: &ast.Case{
						Whens: []ast.CaseAlternative{{
							Cond:   bin("=", lit(int64(1)), lit(int64(1))),
							Result: lit("yes"),
						}},
						Else: lit("no"),
					},
					Alias: "r",
				}}},
			}},
		},
		{
			"with-chaining-and-order",
			"MATCH (p)-[:KNOWS]->(f) WITH p, count(f) AS friends WHERE friends > 5 RETURN p.name AS name ORDER BY name DESC SKIP 2 LIMIT 10",
			&ast.Query{Clauses: []ast.Clause{
				&ast.Match{Parts: []ast.PatternPart{part(node("p", nil, nil),
					ast.PatternChain{Rel: rel("", []string{"KNOWS"}, ast.DirOut), Node: node("f", nil, nil)})}},
				&ast.With{
					Items: []ast.ReturnItem{
						{Expr: vr("p")},
						{Expr: &ast.FunctionCall{Name: "count", Args: []ast.Expr{vr("f")}}, Alias: "friends"},
					},
					Where: bin(">", vr("friends"), lit(int64(5))),
				},
				&ast.Return{
					Items:   []ast.ReturnItem{{Expr: pa(vr("p"), "name"), Alias: "name"}},
					OrderBy: []ast.SortItem{{Expr: vr("name"), Desc: true}},
					Skip:    lit(int64(2)),
					Limit:   lit(int64(10)),
				},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.src, err)
			}
			zeroPos(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AST mismatch\n got: %s\nwant: %s", dump(got), dump(tc.want))
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
		col  int // 0 = not checked
	}{
		{"unclosed-node", "MATCH (n:Person", 1, 16},
		{"return-without-expr", "MATCH (n) RETURN", 1, 0},
		{"trailing-operator", "RETURN 1 +", 1, 0},
		{"unknown-clause", "FOO bar", 1, 1},
		{"empty", "   ", 1, 0},
		{"double-equals", "RETURN 1 = = 2", 1, 0},
		{"unterminated-string", "RETURN 'abc", 1, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) expected an error", tc.src)
			}
			pe, ok := err.(*ParseError)
			if !ok {
				t.Fatalf("expected *ParseError, got %T: %v", err, err)
			}
			if pe.Pos.Line != tc.line {
				t.Errorf("line = %d, want %d (%v)", pe.Pos.Line, tc.line, pe)
			}
			if tc.col != 0 && pe.Pos.Col != tc.col {
				t.Errorf("column = %d, want %d (%v)", pe.Pos.Col, tc.col, pe)
			}
		})
	}
}

// zeroPos recursively zeroes all ast.Pos fields, so the comparison with the
// expected AST ignores source positions.
func zeroPos(v any) { zeroPosValue(reflect.ValueOf(v)) }

var posType = reflect.TypeOf(ast.Pos{})

func zeroPosValue(rv reflect.Value) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !rv.IsNil() {
			zeroPosValue(rv.Elem())
		}
	case reflect.Struct:
		if rv.Type() == posType {
			if rv.CanSet() {
				rv.Set(reflect.Zero(posType))
			}
			return
		}
		for i := 0; i < rv.NumField(); i++ {
			zeroPosValue(rv.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			zeroPosValue(rv.Index(i))
		}
	case reflect.Map:
		for _, k := range rv.MapKeys() {
			zeroPosValue(rv.MapIndex(k))
		}
	}
}

// dump renders the AST readably for the test error messages.
func dump(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}
