package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/expr"
	"github.com/toclocoinc/dynamoat/server/internal/partiql"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() {
	register("ExecuteStatement", (*Ops).executeStatement)
	register("BatchExecuteStatement", (*Ops).batchExecuteStatement)
	register("ExecuteTransaction", (*Ops).executeTransaction)
}

// executeStatementInput is the ExecuteStatement request. It carries only the
// fields DynamoDB's PartiQL operations use; the rest are ignored.
type executeStatementInput struct {
	Statement                           string       `json:"Statement"`
	Parameters                          []attr.Value `json:"Parameters"`
	ConsistentRead                      bool         `json:"ConsistentRead"`
	NextToken                           string       `json:"NextToken"`
	Limit                               *int         `json:"Limit"`
	ReturnConsumedCapacity              string       `json:"ReturnConsumedCapacity"`
	ReturnValuesOnConditionCheckFailure string       `json:"ReturnValuesOnConditionCheckFailure"`
}

func (o *Ops) executeStatement(ctx context.Context, body []byte) (any, error) {
	var req executeStatementInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.Limit != nil && *req.Limit < 1 {
		return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", *req.Limit)
	}
	rvocf, err := checkReturnValuesOnConditionCheckFailure(req.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}
	plan, err := partiql.Parse(req.Statement, req.Parameters)
	if err != nil {
		return nil, err
	}
	switch p := plan.(type) {
	case partiql.SelectPlan:
		return o.execSelect(ctx, p, req)
	case partiql.InsertPlan:
		t, err := o.execInsert(ctx, p)
		if err != nil {
			return nil, err
		}
		return writeResponse(t, req.ReturnConsumedCapacity), nil
	case partiql.UpdatePlan:
		t, err := o.execUpdate(ctx, p, rvocf)
		if err != nil {
			return nil, err
		}
		return writeResponse(t, req.ReturnConsumedCapacity), nil
	case partiql.DeletePlan:
		t, err := o.execDelete(ctx, p, rvocf)
		if err != nil {
			return nil, err
		}
		return writeResponse(t, req.ReturnConsumedCapacity), nil
	case partiql.ExistsPlan:
		return nil, ddberr.Validation("EXISTS is only supported in ExecuteTransaction")
	}
	return nil, ddberr.Validation("Statement wasn't well formed, can't be processed")
}

// writeResponse is the ExecuteStatement response for a successful write: an
// empty Items list (DynamoDB returns {"Items": []} for INSERT, UPDATE and
// DELETE), plus ConsumedCapacity when asked.
func writeResponse(t *storage.Table, rcc string) map[string]any {
	out := map[string]any{"Items": []attr.Item{}}
	if cc := consumedCapacity(t, rcc); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out
}

// partiqlKey is the key condition extracted from a PartiQL WHERE: a partition
// key equality and, optionally, a sort key condition.
type partiqlKey struct {
	hashSet   bool
	hash      attr.Value
	rangeOp   string
	rangeVals []attr.Value
}

// execSelect runs a SELECT as a Query when the WHERE fixes the partition key
// with =, else as a Scan with the whole WHERE as a filter.
func (o *Ops) execSelect(ctx context.Context, p partiql.SelectPlan, req executeStatementInput) (map[string]any, error) {
	t, err := o.table(ctx, p.Table)
	if err != nil {
		return nil, err
	}
	var idx *storage.Index
	if p.Index != "" {
		g, ok := t.GSI(p.Index)
		if !ok {
			return nil, ddberr.Validation("The table does not have the specified index: %s", p.Index)
		}
		idx = g
		if req.ConsistentRead && !idx.Local {
			return nil, ddberr.Validation("Consistent reads are not supported on global secondary indexes")
		}
	}
	if err := checkProjected(t, idx, p.Projection); err != nil {
		return nil, err
	}

	schema := t.Key
	if idx != nil {
		schema = idx.Key
	}
	var esk attr.Item
	if req.NextToken != "" {
		esk, err = nextTokenStartKey(req.NextToken, req.Statement)
		if err != nil {
			return nil, err
		}
		if err := checkExclusiveStartKey(t, idx, esk); err != nil {
			return nil, err
		}
	}

	key, filter := splitWhere(p.Where, schema, p.Values)
	if key.hashSet && key.hash.Type != schema.HashType {
		return nil, keyTypeMismatch()
	}
	if key.rangeOp != "" {
		for _, v := range key.rangeVals {
			if v.Type != schema.RangeType {
				return nil, keyTypeMismatch()
			}
		}
		if key.rangeOp == "begins_with" && schema.RangeType != attr.TypeS && schema.RangeType != attr.TypeB {
			return nil, ddberr.Validation("One or more parameter values were invalid: Invalid operator used in KeyConditionExpression: begins_with")
		}
	}

	var page storage.Page
	limit := storage.PageLimit{Bytes: pageBytes}
	if req.Limit != nil {
		limit.Items = *req.Limit
	}
	if key.hashSet {
		in := storage.QueryInput{
			Table:      t,
			Index:      p.Index,
			Hash:       key.hash,
			Forward:    true,
			StartAfter: esk,
			Limit:      limit,
		}
		if key.rangeOp != "" {
			in.Range = &storage.RangeCond{Op: key.rangeOp, Values: key.rangeVals}
		}
		page, err = o.Store.Query(ctx, in)
	} else {
		in := storage.ScanInput{
			Table:      t,
			Index:      p.Index,
			StartAfter: esk,
			Limit:      limit,
		}
		page, err = o.Store.Scan(ctx, in)
	}
	if err != nil {
		return nil, err
	}

	items := make([]attr.Item, 0, len(page.Items))
	for _, it := range page.Items {
		out := it
		// A non-ALL GSI returns only its projected attributes; an LSI read with
		// SELECT * fetches the whole base item instead.
		if idx != nil && idx.Projection != storage.ProjectAll && !(idx.Local && p.Projection == nil) {
			out = indexProjection(t, idx, out)
		}
		if filter != nil {
			ok, err := expr.Eval(filter, out, p.Values, "FilterExpression")
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if p.Projection != nil {
			out = expr.Project(out, p.Projection)
		}
		items = append(items, out)
	}

	resp := map[string]any{"Items": items}
	if page.LastKey != nil {
		resp["LastEvaluatedKey"] = page.LastKey
		tok, err := makeNextToken(req.Statement, page.LastKey)
		if err != nil {
			return nil, err
		}
		resp["NextToken"] = tok
	}
	if cc := consumedCapacity(t, req.ReturnConsumedCapacity); cc != nil {
		resp["ConsumedCapacity"] = cc
	}
	return resp, nil
}

// execInsert stores an item only when its key does not exist, as DynamoDB's
// PartiQL INSERT does. An existing item is DuplicateItemException.
func (o *Ops) execInsert(ctx context.Context, p partiql.InsertPlan) (*storage.Table, error) {
	t, err := o.table(ctx, p.Table)
	if err != nil {
		return nil, err
	}
	key, err := storage.KeyOf(t.Key, attr.Item(p.Item), true)
	if err != nil {
		return nil, err
	}
	if err := checkIndexKeys(t, p.Item); err != nil {
		return nil, err
	}
	if err := attr.CheckItemSize(p.Item); err != nil {
		return nil, err
	}
	_, _, err = o.Store.WriteItem(ctx, storage.Write{
		Table: t,
		Key:   key,
		Fn: func(cur attr.Item) (storage.WriteResult, error) {
			if cur != nil {
				return storage.WriteResult{}, ddberr.DuplicateItem()
			}
			return storage.WriteResult{Op: storage.Put, Item: p.Item}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// execUpdate applies a PartiQL UPDATE. The WHERE must fix the full primary key
// with =; the remaining conditions are the ConditionExpression. An UPDATE of a
// missing item is ConditionalCheckFailedException (PartiQL UPDATE does not
// upsert).
func (o *Ops) execUpdate(ctx context.Context, p partiql.UpdatePlan, rvocf string) (*storage.Table, error) {
	t, err := o.table(ctx, p.Table)
	if err != nil {
		return nil, err
	}
	key, cond, err := requireFullKey(p.Where, t.Key, p.Values)
	if err != nil {
		return nil, err
	}
	if name, ok := keyPath(p.Update, t.Key); ok {
		return nil, ddberr.Validation("One or more parameter values were invalid: Cannot update attribute %s. This attribute is part of the key", name)
	}
	_, _, err = o.Store.WriteItem(ctx, storage.Write{
		Table: t,
		Key:   key,
		Fn: func(cur attr.Item) (storage.WriteResult, error) {
			if cur == nil {
				return storage.WriteResult{}, conditionalCheckFailed(nil, rvocf)
			}
			if cond != nil {
				ok, err := expr.Eval(cond, cur, p.Values, "ConditionExpression")
				if err != nil {
					return storage.WriteResult{}, err
				}
				if !ok {
					return storage.WriteResult{}, conditionalCheckFailed(cur, rvocf)
				}
			}
			updated, _, err := expr.Apply(p.Update, cur, p.Values)
			if err != nil {
				return storage.WriteResult{}, err
			}
			if err := checkIndexKeys(t, updated); err != nil {
				return storage.WriteResult{}, err
			}
			if err := attr.CheckItemSize(updated); err != nil {
				return storage.WriteResult{}, err
			}
			return storage.WriteResult{Op: storage.Put, Item: updated}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// execDelete deletes the item fixed by a PartiQL DELETE's WHERE; extra
// conditions become the ConditionExpression. Deleting a missing item is a no-op.
func (o *Ops) execDelete(ctx context.Context, p partiql.DeletePlan, rvocf string) (*storage.Table, error) {
	t, err := o.table(ctx, p.Table)
	if err != nil {
		return nil, err
	}
	key, cond, err := requireFullKey(p.Where, t.Key, p.Values)
	if err != nil {
		return nil, err
	}
	_, _, err = o.Store.WriteItem(ctx, storage.Write{
		Table: t,
		Key:   key,
		Fn: func(cur attr.Item) (storage.WriteResult, error) {
			if cond != nil {
				ok, err := expr.Eval(cond, cur, p.Values, "ConditionExpression")
				if err != nil {
					return storage.WriteResult{}, err
				}
				if !ok {
					return storage.WriteResult{}, conditionalCheckFailed(cur, rvocf)
				}
			}
			return storage.WriteResult{Op: storage.Delete}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// flattenAnd returns the conjuncts of an AND chain, in source order.
func flattenAnd(c expr.Cond) []expr.Cond {
	if c == nil {
		return nil
	}
	if a, ok := c.(expr.And); ok {
		return append(flattenAnd(a.L), flattenAnd(a.R)...)
	}
	return []expr.Cond{c}
}

// andAll rebuilds an AND chain from conjuncts; nil for none.
func andAll(cs []expr.Cond) expr.Cond {
	if len(cs) == 0 {
		return nil
	}
	c := cs[0]
	for _, n := range cs[1:] {
		c = expr.And{L: c, R: n}
	}
	return c
}

// equalityOn reports whether c is `name = :value`, returning the placeholder.
func equalityOn(c expr.Cond, name string) (string, bool) {
	cmp, ok := c.(expr.Compare)
	if !ok || cmp.Op != "=" {
		return "", false
	}
	l, ok := cmp.L.(expr.PathOperand)
	if !ok || len(l.Path) != 1 || l.Path[0].Name != name {
		return "", false
	}
	r, ok := cmp.R.(expr.ValueOperand)
	if !ok {
		return "", false
	}
	return r.Name, true
}

// sortKeyCond reports whether c is a sort key condition the Query can push
// down, returning the operator and the value placeholders.
func sortKeyCond(c expr.Cond, name string) (op string, names []string, ok bool) {
	switch c := c.(type) {
	case expr.Compare:
		switch c.Op {
		case "=", "<", "<=", ">", ">=":
		default:
			return "", nil, false
		}
		l, ok := c.L.(expr.PathOperand)
		if !ok || len(l.Path) != 1 || l.Path[0].Name != name {
			return "", nil, false
		}
		r, ok := c.R.(expr.ValueOperand)
		if !ok {
			return "", nil, false
		}
		return c.Op, []string{r.Name}, true
	case expr.Between:
		l, ok := c.V.(expr.PathOperand)
		if !ok || len(l.Path) != 1 || l.Path[0].Name != name {
			return "", nil, false
		}
		lo, ok1 := c.Lo.(expr.ValueOperand)
		hi, ok2 := c.Hi.(expr.ValueOperand)
		if !ok1 || !ok2 {
			return "", nil, false
		}
		return "BETWEEN", []string{lo.Name, hi.Name}, true
	case expr.Func:
		if c.Name != "begins_with" || len(c.Args) != 2 {
			return "", nil, false
		}
		l, ok := c.Args[0].(expr.PathOperand)
		if !ok || len(l.Path) != 1 || l.Path[0].Name != name {
			return "", nil, false
		}
		r, ok := c.Args[1].(expr.ValueOperand)
		if !ok {
			return "", nil, false
		}
		return "begins_with", []string{r.Name}, true
	}
	return "", nil, false
}

// splitWhere extracts the partition key equality (and, optionally, a sort key
// condition) from a PartiQL WHERE and returns the remaining filter.
func splitWhere(where expr.Cond, schema storage.KeySchema, values map[string]attr.Value) (partiqlKey, expr.Cond) {
	var key partiqlKey
	var rest []expr.Cond
	for _, c := range flattenAnd(where) {
		if !key.hashSet {
			if n, ok := equalityOn(c, schema.Hash); ok {
				key.hashSet = true
				key.hash = values[n]
				continue
			}
		}
		rest = append(rest, c)
	}
	// A sort key condition can only be pushed down once the partition key is
	// fixed; it may come before or after that conjunct in the WHERE.
	if key.hashSet && schema.Range != "" {
		kept := make([]expr.Cond, 0, len(rest))
		for _, c := range rest {
			if key.rangeOp == "" {
				if op, names, ok := sortKeyCond(c, schema.Range); ok {
					vals := make([]attr.Value, len(names))
					for i, n := range names {
						vals[i] = values[n]
					}
					key.rangeOp = op
					key.rangeVals = vals
					continue
				}
			}
			kept = append(kept, c)
		}
		rest = kept
	}
	return key, andAll(rest)
}

// requireFullKey extracts the full primary key from a PartiQL WHERE; every key
// attribute must be fixed with =. The remaining conditions are returned.
func requireFullKey(where expr.Cond, schema storage.KeySchema, values map[string]attr.Value) (storage.Key, expr.Cond, error) {
	var hashName, rangeName string
	var rest []expr.Cond
	for _, c := range flattenAnd(where) {
		if hashName == "" {
			if n, ok := equalityOn(c, schema.Hash); ok {
				hashName = n
				continue
			}
		}
		if schema.Range != "" && rangeName == "" {
			if n, ok := equalityOn(c, schema.Range); ok {
				rangeName = n
				continue
			}
		}
		rest = append(rest, c)
	}
	if hashName == "" || (schema.Range != "" && rangeName == "") {
		return storage.Key{}, nil, ddberr.Validation("Where clause does not contain a mandatory equality on all key attributes")
	}
	item := attr.Item{schema.Hash: values[hashName]}
	if schema.Range != "" {
		item[schema.Range] = values[rangeName]
	}
	key, err := storage.KeyOf(schema, item, false)
	if err != nil {
		return storage.Key{}, nil, err
	}
	return key, andAll(rest), nil
}

// makeNextToken encodes an ExclusiveStartKey and the statement's hash, so a
// token is rejected when presented to a different statement.
func makeNextToken(statement string, esk attr.Item) (string, error) {
	sum := sha256.Sum256([]byte(statement))
	b, err := json.Marshal(esk)
	if err != nil {
		return "", err
	}
	payload := make([]byte, 0, len(sum)+len(b))
	payload = append(payload, sum[:]...)
	payload = append(payload, b...)
	return base64.StdEncoding.EncodeToString(payload), nil
}

// nextTokenStartKey decodes a NextToken and checks its statement hash.
func nextTokenStartKey(tok, statement string) (attr.Item, error) {
	raw, err := base64.StdEncoding.DecodeString(tok)
	if err != nil || len(raw) <= sha256.Size {
		return nil, ddberr.Validation("Invalid NextToken")
	}
	want := sha256.Sum256([]byte(statement))
	if !bytes.Equal(raw[:sha256.Size], want[:]) {
		return nil, ddberr.Validation("Invalid NextToken")
	}
	var esk attr.Item
	if err := json.Unmarshal(raw[sha256.Size:], &esk); err != nil {
		return nil, ddberr.Validation("Invalid NextToken")
	}
	return esk, nil
}

// batchStatement is one statement of a BatchExecuteStatement request.
type batchStatement struct {
	Statement      string       `json:"Statement"`
	Parameters     []attr.Value `json:"Parameters"`
	ConsistentRead bool         `json:"ConsistentRead"`
}

type batchExecuteStatementInput struct {
	Statements []batchStatement `json:"Statements"`
}

func (o *Ops) batchExecuteStatement(ctx context.Context, body []byte) (any, error) {
	var req batchExecuteStatementInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if len(req.Statements) < 1 || len(req.Statements) > 25 {
		raw := rawBatchStatements(body)
		if len(req.Statements) < 1 {
			return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'statements' failed to satisfy constraint: Member must have length greater than or equal to 1", raw)
		}
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'statements' failed to satisfy constraint: Member must have length less than or equal to 25", raw)
	}

	plans := make([]partiql.Statement, len(req.Statements))
	reads, writes := 0, 0
	for i := range req.Statements {
		s, err := partiql.Parse(req.Statements[i].Statement, req.Statements[i].Parameters)
		if err != nil {
			return nil, err
		}
		plans[i] = s
		switch s.(type) {
		case partiql.SelectPlan:
			reads++
		case partiql.ExistsPlan:
			return nil, ddberr.Validation("EXISTS is only supported in ExecuteTransaction")
		default:
			writes++
		}
	}
	if reads > 0 && writes > 0 {
		return nil, ddberr.Validation("All statements in a BatchExecuteStatement request must be either read statements or write statements")
	}

	responses := make([]map[string]any, len(plans))
	for i := range plans {
		responses[i] = o.batchStatementResponse(ctx, plans[i], req.Statements[i].ConsistentRead)
	}
	return map[string]any{"Responses": responses}, nil
}

// batchStatementResponse runs one batch statement and shapes its per-statement
// response: {TableName, Item?} for a read, {TableName} for a write, or
// {Error: {Code, Message}} on failure.
func (o *Ops) batchStatementResponse(ctx context.Context, s partiql.Statement, consistentRead bool) map[string]any {
	switch p := s.(type) {
	case partiql.SelectPlan:
		t, err := o.table(ctx, p.Table)
		if err != nil {
			return batchErrorResponse(err)
		}
		if consistentRead {
			if g, ok := t.GSI(p.Index); ok && !g.Local {
				return batchErrorResponse(ddberr.Validation("Consistent reads are not supported on global secondary indexes"))
			}
		}
		key, rest, err := requireFullKey(p.Where, t.Key, p.Values)
		if err != nil {
			return batchErrorResponse(err)
		}
		if rest != nil {
			return batchErrorResponse(ddberr.Validation("Where clause does not contain a mandatory equality on all key attributes"))
		}
		item, err := o.Store.GetItem(ctx, t, key)
		if err != nil {
			return batchErrorResponse(err)
		}
		resp := map[string]any{"TableName": t.Name}
		if item != nil {
			if p.Projection != nil {
				item = expr.Project(item, p.Projection)
			}
			resp["Item"] = item
		}
		return resp
	case partiql.InsertPlan:
		t, err := o.execInsert(ctx, p)
		if err != nil {
			return batchErrorResponse(err)
		}
		return map[string]any{"TableName": t.Name}
	case partiql.UpdatePlan:
		t, err := o.execUpdate(ctx, p, "")
		if err != nil {
			return batchErrorResponse(err)
		}
		return map[string]any{"TableName": t.Name}
	case partiql.DeletePlan:
		t, err := o.execDelete(ctx, p, "")
		if err != nil {
			return batchErrorResponse(err)
		}
		return map[string]any{"TableName": t.Name}
	}
	return batchErrorResponse(ddberr.Validation("Statement wasn't well formed, can't be processed"))
}

// batchErrorResponse turns an error into a batch response entry's error code.
func batchErrorResponse(err error) map[string]any {
	code := "InternalServerError"
	msg := "Internal server error"
	var de *ddberr.Error
	if errors.As(err, &de) {
		code = batchErrorCode(de.Code())
		msg = de.Message
	} else {
		msg = err.Error()
	}
	return map[string]any{"Error": map[string]any{"Code": code, "Message": msg}}
}

// batchErrorCode maps a ddberr code to DynamoDB's BatchExecuteStatement error
// code naming.
func batchErrorCode(code string) string {
	switch code {
	case "ValidationException":
		return "ValidationError"
	case "ResourceNotFoundException":
		return "ResourceNotFound"
	case "ConditionalCheckFailedException":
		return "ConditionalCheckFailed"
	case "DuplicateItemException":
		return "DuplicateItem"
	case "ItemCollectionSizeLimitExceededException":
		return "ItemCollectionSizeLimitExceeded"
	case "ProvisionedThroughputExceededException":
		return "ProvisionedThroughputExceeded"
	case "TransactionConflictException":
		return "TransactionConflict"
	case "ThrottlingException":
		return "ThrottlingError"
	case "IdempotentParameterMismatchException":
		return "IdempotentParameterMismatch"
	}
	if len(code) > len("Exception") && code[len(code)-len("Exception"):] == "Exception" {
		return code[:len(code)-len("Exception")]
	}
	return code
}

// rawBatchStatements returns the Statements member verbatim, for validation
// messages that echo the offending value.
func rawBatchStatements(body []byte) string {
	var raw struct {
		Statements json.RawMessage `json:"Statements"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Statements) == 0 {
		return "[]"
	}
	return string(raw.Statements)
}

// executeTransactionInput is the ExecuteTransaction request.
type executeTransactionInput struct {
	TransactStatements []struct {
		Statement  string       `json:"Statement"`
		Parameters []attr.Value `json:"Parameters"`
	} `json:"TransactStatements"`
	ClientRequestToken string `json:"ClientRequestToken"`
}

func (o *Ops) executeTransaction(ctx context.Context, body []byte) (any, error) {
	var req executeTransactionInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.ClientRequestToken != "" && len(req.ClientRequestToken) > 36 {
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'clientRequestToken' failed to satisfy constraint: Member must have length less than or equal to 36", req.ClientRequestToken)
	}
	if len(req.TransactStatements) < 1 || len(req.TransactStatements) > 100 {
		raw := rawTransactStatements(body)
		if len(req.TransactStatements) < 1 {
			return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'transactStatements' failed to satisfy constraint: Member must have length greater than or equal to 1", raw)
		}
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'transactStatements' failed to satisfy constraint: Member must have length less than or equal to 100", raw)
	}

	plans := make([]partiql.Statement, len(req.TransactStatements))
	reads, writes := 0, 0
	for i := range req.TransactStatements {
		s, err := partiql.Parse(req.TransactStatements[i].Statement, req.TransactStatements[i].Parameters)
		if err != nil {
			return nil, err
		}
		plans[i] = s
		if _, ok := s.(partiql.SelectPlan); ok {
			reads++
		} else {
			writes++
		}
	}
	if reads > 0 && writes > 0 {
		return nil, ddberr.Validation("Transaction statements must be either read statements or write statements")
	}
	if reads > 0 {
		return o.execTransactionReads(ctx, plans)
	}
	return o.execTransactionWrites(ctx, body, plans, req.ClientRequestToken)
}

// partiqlTxEntry is one validated ExecuteTransaction write element.
type partiqlTxEntry struct {
	kind   string
	table  *storage.Table
	key    storage.Key
	item   attr.Item // the item of a Put
	cond   expr.Cond
	update expr.Update
	values map[string]attr.Value
	rvocf  string
	old    attr.Item
}

// execTransactionReads runs an all-reads transaction like TransactGetItems.
func (o *Ops) execTransactionReads(ctx context.Context, plans []partiql.Statement) (any, error) {
	type entry struct {
		table *storage.Table
		key   storage.Key
		proj  []expr.Path
		item  attr.Item
	}
	entries := make([]entry, len(plans))
	tables := make([]*storage.Table, 0, 4)
	seen := make(map[string]bool, len(plans))
	seenTable := make(map[string]bool, len(plans))
	for i, s := range plans {
		p := s.(partiql.SelectPlan)
		t, err := o.table(ctx, p.Table)
		if err != nil {
			return nil, err
		}
		key, rest, err := requireFullKey(p.Where, t.Key, p.Values)
		if err != nil {
			return nil, err
		}
		if rest != nil {
			return nil, ddberr.Validation("Where clause does not contain a mandatory equality on all key attributes")
		}
		enc := t.Name + "\x00" + encodeStorageKey(key)
		if seen[enc] {
			return nil, ddberr.Validation("Transaction request cannot include multiple operations on one item")
		}
		seen[enc] = true
		if !seenTable[t.Name] {
			seenTable[t.Name] = true
			tables = append(tables, t)
		}
		entries[i] = entry{table: t, key: key, proj: p.Projection}
	}

	writes := make([]storage.Write, len(entries))
	for i := range entries {
		e := &entries[i]
		writes[i] = storage.Write{Table: e.table, Key: e.key, Fn: func(cur attr.Item) (storage.WriteResult, error) {
			e.item = cur
			return storage.WriteResult{Op: storage.Keep}, nil
		}}
	}
	if err := o.Store.Transact(ctx, writes); err != nil {
		var tx *storage.TxError
		if errors.As(err, &tx) && errors.Is(tx.Err, storage.ErrThrottled) {
			reasons := make([]map[string]any, len(entries))
			for i := range reasons {
				reasons[i] = map[string]any{"Code": "None"}
			}
			reasons[tx.Index] = map[string]any{"Code": "TransactionConflict", "Message": "Transaction is ongoing for the item"}
			return nil, ddberr.TransactionCanceled(reasons)
		}
		return nil, err
	}

	responses := make([]map[string]any, len(entries))
	total := 0
	for i := range entries {
		it := entries[i].item
		if it == nil {
			responses[i] = map[string]any{}
			continue
		}
		if entries[i].proj != nil {
			it = expr.Project(it, entries[i].proj)
		}
		if total += attr.ItemSize(it); total > transactGetLimit {
			return nil, ddberr.Validation("Transaction request cannot include more than 4 MB of data")
		}
		responses[i] = map[string]any{"Item": it}
	}
	return map[string]any{"Responses": responses}, nil
}

// execTransactionWrites runs an all-writes transaction through the same
// Transact path as TransactWriteItems; EXISTS becomes a ConditionCheck.
func (o *Ops) execTransactionWrites(ctx context.Context, body []byte, plans []partiql.Statement, token string) (any, error) {
	entries, err := o.partiqlTxPlans(ctx, plans)
	if err != nil {
		return nil, err
	}

	if token != "" {
		hit, err := o.transactIdempotent(token, body)
		if err != nil {
			return nil, err
		}
		if hit {
			return map[string]any{"Responses": []map[string]any{}}, nil
		}
	}

	writes := make([]storage.Write, len(entries))
	for i := range entries {
		writes[i] = entries[i].write()
	}
	if err := o.Store.Transact(ctx, writes); err != nil {
		if token != "" {
			o.forgetIdempotent(token)
		}
		var tx *storage.TxError
		if !errors.As(err, &tx) {
			return nil, err
		}
		var de *ddberr.Error
		if !errors.As(tx.Err, &de) && !errors.Is(tx.Err, storage.ErrThrottled) {
			return nil, tx.Err
		}
		return nil, partiqlTransactCanceled(entries, tx.Index, tx.Err)
	}
	return map[string]any{"Responses": []map[string]any{}}, nil
}

// partiqlTxPlans validates every statement and resolves its table, key and
// conditions before any write.
func (o *Ops) partiqlTxPlans(ctx context.Context, plans []partiql.Statement) ([]partiqlTxEntry, error) {
	entries := make([]partiqlTxEntry, 0, len(plans))
	seen := make(map[string]bool, len(plans))
	for _, s := range plans {
		e, err := o.partiqlTxEntry(ctx, s)
		if err != nil {
			return nil, err
		}
		enc := e.table.Name + "\x00" + encodeStorageKey(e.key)
		if seen[enc] {
			return nil, ddberr.Validation("Transaction request cannot include multiple operations on one item")
		}
		seen[enc] = true
		entries = append(entries, e)
	}
	return entries, nil
}

// partiqlTxEntry validates one statement and returns its plan.
func (o *Ops) partiqlTxEntry(ctx context.Context, s partiql.Statement) (partiqlTxEntry, error) {
	var e partiqlTxEntry
	switch p := s.(type) {
	case partiql.InsertPlan:
		t, err := o.table(ctx, p.Table)
		if err != nil {
			return e, err
		}
		key, err := storage.KeyOf(t.Key, attr.Item(p.Item), true)
		if err != nil {
			return e, err
		}
		if err := attr.CheckItemSize(p.Item); err != nil {
			return e, err
		}
		return partiqlTxEntry{kind: "Put", table: t, key: key, item: attr.Item(p.Item)}, nil

	case partiql.UpdatePlan:
		t, err := o.table(ctx, p.Table)
		if err != nil {
			return e, err
		}
		key, cond, err := requireFullKey(p.Where, t.Key, p.Values)
		if err != nil {
			return e, err
		}
		if name, ok := keyPath(p.Update, t.Key); ok {
			return e, ddberr.Validation("One or more parameter values were invalid: Cannot update attribute %s. This attribute is part of the key", name)
		}
		return partiqlTxEntry{kind: "Update", table: t, key: key, cond: cond, update: p.Update, values: p.Values}, nil

	case partiql.DeletePlan:
		t, err := o.table(ctx, p.Table)
		if err != nil {
			return e, err
		}
		key, cond, err := requireFullKey(p.Where, t.Key, p.Values)
		if err != nil {
			return e, err
		}
		return partiqlTxEntry{kind: "Delete", table: t, key: key, cond: cond, values: p.Values}, nil

	case partiql.ExistsPlan:
		t, err := o.table(ctx, p.Select.Table)
		if err != nil {
			return e, err
		}
		key, extra, err := requireFullKey(p.Select.Where, t.Key, p.Select.Values)
		if err != nil {
			return e, err
		}
		exists := expr.Func{Name: "attribute_exists", Args: []expr.Operand{expr.PathOperand{Path: expr.Path{{Name: t.Key.Hash}}}}}
		cond := expr.Cond(exists)
		if extra != nil {
			cond = expr.And{L: exists, R: extra}
		}
		return partiqlTxEntry{kind: "ConditionCheck", table: t, key: key, cond: cond, values: p.Select.Values}, nil
	}
	return e, ddberr.Validation("Statement wasn't well formed, can't be processed")
}

// write builds the storage.Write for a validated ExecuteTransaction entry.
func (e *partiqlTxEntry) write() storage.Write {
	return storage.Write{Table: e.table, Key: e.key, Fn: func(cur attr.Item) (storage.WriteResult, error) {
		e.old = cur
		switch e.kind {
		case "Put":
			if cur != nil {
				return storage.WriteResult{}, ddberr.DuplicateItem()
			}
			if err := checkIndexKeys(e.table, e.item); err != nil {
				return storage.WriteResult{}, err
			}
			return storage.WriteResult{Op: storage.Put, Item: e.item}, nil
		case "Update":
			if cur == nil {
				return storage.WriteResult{}, conditionalCheckFailed(nil, e.rvocf)
			}
			if e.cond != nil {
				ok, err := expr.Eval(e.cond, cur, e.values, "ConditionExpression")
				if err != nil {
					return storage.WriteResult{}, err
				}
				if !ok {
					return storage.WriteResult{}, conditionalCheckFailed(cur, e.rvocf)
				}
			}
			updated, _, err := expr.Apply(e.update, cur, e.values)
			if err != nil {
				return storage.WriteResult{}, err
			}
			if err := checkIndexKeys(e.table, updated); err != nil {
				return storage.WriteResult{}, err
			}
			if err := attr.CheckItemSize(updated); err != nil {
				return storage.WriteResult{}, err
			}
			return storage.WriteResult{Op: storage.Put, Item: updated}, nil
		case "Delete":
			if e.cond != nil {
				ok, err := expr.Eval(e.cond, cur, e.values, "ConditionExpression")
				if err != nil {
					return storage.WriteResult{}, err
				}
				if !ok {
					return storage.WriteResult{}, conditionalCheckFailed(cur, e.rvocf)
				}
			}
			return storage.WriteResult{Op: storage.Delete}, nil
		case "ConditionCheck":
			ok, err := expr.Eval(e.cond, cur, e.values, "ConditionExpression")
			if err != nil {
				return storage.WriteResult{}, err
			}
			if !ok {
				return storage.WriteResult{}, conditionalCheckFailed(cur, e.rvocf)
			}
			return storage.WriteResult{Op: storage.Keep}, nil
		}
		return storage.WriteResult{}, nil
	}}
}

// partiqlTransactCanceled turns a *storage.TxError into TransactionCanceled with
// one reason per entry in request order.
func partiqlTransactCanceled(entries []partiqlTxEntry, failed int, err error) error {
	reasons := make([]map[string]any, len(entries))
	for i := range entries {
		if i != failed {
			reasons[i] = map[string]any{"Code": "None"}
			continue
		}
		reasons[i] = partiqlTransactReason(entries[i], err)
	}
	return ddberr.TransactionCanceled(reasons)
}

// partiqlTransactReason reports why one entry failed.
func partiqlTransactReason(e partiqlTxEntry, err error) map[string]any {
	if errors.Is(err, storage.ErrThrottled) {
		return map[string]any{"Code": "TransactionConflict", "Message": "Transaction is ongoing for the item"}
	}
	var de *ddberr.Error
	if errors.As(err, &de) {
		switch de.Code() {
		case "ConditionalCheckFailedException":
			r := map[string]any{"Code": "ConditionalCheckFailed", "Message": "The conditional request failed"}
			if e.rvocf == returnAllOld && e.old != nil {
				r["Item"] = itemJSON(e.old)
			}
			return r
		case "DuplicateItemException":
			return map[string]any{"Code": "DuplicateItem", "Message": de.Message}
		}
		return map[string]any{"Code": "ValidationError", "Message": de.Message}
	}
	return map[string]any{"Code": "ValidationError", "Message": err.Error()}
}

// rawTransactStatements returns the TransactStatements member verbatim.
func rawTransactStatements(body []byte) string {
	var raw struct {
		TransactStatements json.RawMessage `json:"TransactStatements"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.TransactStatements) == 0 {
		return "[]"
	}
	return string(raw.TransactStatements)
}
