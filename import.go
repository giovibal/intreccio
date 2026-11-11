package intreccio

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/sema"
)

// DefaultImportBatch is the number of rows Import writes per transaction
// unless WithBatchSize says otherwise.
const DefaultImportBatch = 10_000

// ImportOption configures Import.
type ImportOption func(*importOptions)

type importOptions struct{ batch int }

// WithBatchSize sets how many rows Import writes per transaction (default
// DefaultImportBatch). Import still halves it when a batch is too large.
func WithBatchSize(n int) ImportOption {
	return func(o *importOptions) {
		if n > 0 {
			o.batch = n
		}
	}
}

// ImportResult reports how far an Import got.
type ImportResult struct {
	// Rows is the number of rows committed: the rows before the first
	// failed batch, all of them on success.
	Rows int
	// Batches is the number of transactions committed.
	Batches int
}

// Import writes a large number of rows with one write query, in batches of
// separate transactions, so it is not bound by the size of one transaction.
// The query receives each batch as the $rows parameter, next to params:
//
//	res, err := db.Import(ctx,
//	    "UNWIND $rows AS r CREATE (p:Person) SET p = r", people, nil)
//
// rows is a slice of any type whose elements are valid parameter values
// (typically []map[string]any). When a batch exceeds what one transaction
// can hold (ErrTxnTooLarge), Import halves the batch size and retries it, and
// keeps the smaller size. Each batch is atomic, but the import as a whole is
// not: when a batch fails, the batches before it stay committed. The error
// then names the failed rows (by index in rows) and ImportResult.Rows says
// how many were committed, so the caller can resume from there. In cluster
// mode every batch is a replicated write, forwarded to the leader as usual.
func (db *DB) Import(ctx context.Context, cypher string, rows any, params map[string]any, opts ...ImportOption) (ImportResult, error) {
	var res ImportResult
	o := importOptions{batch: DefaultImportBatch}
	for _, opt := range opts {
		opt(&o)
	}
	rv := reflect.ValueOf(rows)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return res, fmt.Errorf("import: rows must be a slice, got %T", rows)
	}
	if _, taken := params["rows"]; taken {
		return res, errors.New(`import: params must not contain "rows": Import sets it to each batch`)
	}
	q, err := parser.Parse(cypher)
	if err != nil {
		return res, err
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return res, err
	}
	if !isWriteQuery(q) {
		return res, errors.New("import: the query must write (CREATE, MERGE, SET, ...)")
	}
	if !slices.Contains(semaRes.Params, "rows") {
		return res, errors.New("import: the query must read the batch from $rows (e.g. UNWIND $rows AS r CREATE ...)")
	}

	all := make([]any, rv.Len())
	for i := range all {
		all[i] = rv.Index(i).Interface()
	}
	batchParams := make(map[string]any, len(params)+1)
	for k, v := range params {
		batchParams[k] = v
	}
	size := o.batch
	for res.Rows < len(all) {
		end := min(res.Rows+size, len(all))
		batchParams["rows"] = all[res.Rows:end]
		_, err := db.Query(ctx, cypher, batchParams)
		switch {
		case err == nil:
			res.Rows, res.Batches = end, res.Batches+1
		case errors.Is(err, ErrTxnTooLarge) && size > 1:
			size /= 2 // nothing was committed: retry the same rows in smaller batches
		default:
			return res, fmt.Errorf("import: rows %d to %d: %w", res.Rows, end-1, err)
		}
	}
	return res, nil
}
