package ops

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

const typTransactionCanceled = "com.amazonaws.dynamodb.v20120810#TransactionCanceledException"
const typIdempotentMismatch = "com.amazonaws.dynamodb.v20120810#IdempotentParameterMismatchException"

// makeLedger creates the accounts and ledger tables the transfer tests use.
func makeLedger(t *testing.T, store *memstore.Store) {
	t.Helper()
	makeTable(t, store, "accounts", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
	makeTable(t, store, "ledger", storage.KeySchema{Hash: "id", HashType: attr.TypeS})
}

// putItem posts a PutItem for one item, for test setup.
func putItem(t *testing.T, h *api.Handler, table, itemJSON string) {
	t.Helper()
	rec := post(t, h, "PutItem", `{"TableName":"`+table+`","Item":`+itemJSON+`}`)
	if rec.Code != 200 {
		t.Fatalf("PutItem setup: status %d; body: %s", rec.Code, rec.Body.String())
	}
}

// transferBody builds the JSON of a ppmMoney-style transfer of amt from one
// account to another, with a ledger row.
func transferBody(from, to, ledgerID, amt string) string {
	return `{"TransactItems":[` +
		`{"Update":{"TableName":"accounts","Key":{"id":{"S":"` + from + `"}},"UpdateExpression":"SET balance = balance - :amt","ConditionExpression":"balance >= :amt","ExpressionAttributeValues":{":amt":{"N":"` + amt + `"}}}},` +
		`{"Update":{"TableName":"accounts","Key":{"id":{"S":"` + to + `"}},"UpdateExpression":"SET balance = balance + :amt","ExpressionAttributeValues":{":amt":{"N":"` + amt + `"}}}},` +
		`{"Put":{"TableName":"ledger","Item":{"id":{"S":"` + ledgerID + `"},"amt":{"N":"` + amt + `"},"from":{"S":"` + from + `"},"to":{"S":"` + to + `"}}}}` +
		`]}`
}

// balance reads the balance attribute of an account, or "" when absent.
func balance(t *testing.T, h *api.Handler, id string) string {
	t.Helper()
	rec := post(t, h, "GetItem", `{"TableName":"accounts","Key":{"id":{"S":"`+id+`"}}}`)
	m := wantStatus(t, rec, 200)
	item, _ := m["Item"].(map[string]any)
	if item == nil {
		return ""
	}
	bal, _ := item["balance"].(map[string]any)
	if bal == nil {
		return ""
	}
	n, _ := bal["N"].(string)
	return n
}

func TestTransactTransferSuccessThenConditionFailure(t *testing.T) {
	_, h, store := newOps(t)
	makeLedger(t, store)
	putItem(t, h, "accounts", `{"id":{"S":"a"},"balance":{"N":"100"}}`)
	putItem(t, h, "accounts", `{"id":{"S":"b"},"balance":{"N":"0"}}`)

	wantStatus(t, post(t, h, "TransactWriteItems", transferBody("a", "b", "l1", "30")), 200)
	if got := balance(t, h, "a"); got != "70" {
		t.Errorf("a balance = %q, want 70", got)
	}
	if got := balance(t, h, "b"); got != "30" {
		t.Errorf("b balance = %q, want 30", got)
	}

	// A transfer larger than the balance fails the debit condition and must
	// leave every item untouched.
	rec := post(t, h, "TransactWriteItems", transferBody("a", "b", "l2", "500"))
	wantErr(t, rec, typTransactionCanceled, "Transaction cancelled, please refer cancellation reasons for specific reasons [ConditionalCheckFailed, None, None]")
	m := bodyJSON(t, rec)
	equalJSON(t, m["CancellationReasons"], []any{
		map[string]any{"Code": "ConditionalCheckFailed", "Message": "The conditional request failed"},
		map[string]any{"Code": "None"},
		map[string]any{"Code": "None"},
	}, "CancellationReasons")

	if got := balance(t, h, "a"); got != "70" {
		t.Errorf("a balance after failure = %q, want 70", got)
	}
	if got := balance(t, h, "b"); got != "30" {
		t.Errorf("b balance after failure = %q, want 30", got)
	}
	rec = post(t, h, "GetItem", `{"TableName":"ledger","Key":{"id":{"S":"l2"}}}`)
	m = wantStatus(t, rec, 200)
	if _, ok := m["Item"]; ok {
		t.Errorf("ledger l2 = %v, want absent", m["Item"])
	}
}

func TestTransactConditionCheck(t *testing.T) {
	_, h, store := newOps(t)
	makeLedger(t, store)
	putItem(t, h, "accounts", `{"id":{"S":"a"},"balance":{"N":"5"}}`)

	// A ConditionCheck that holds writes nothing.
	rec := post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"ConditionCheck":{"TableName":"accounts","Key":{"id":{"S":"a"}},"ConditionExpression":"balance >= :amt","ExpressionAttributeValues":{":amt":{"N":"5"}}}}`+
		`]}`)
	wantStatus(t, rec, 200)

	// A failing ConditionCheck cancels the transaction.
	rec = post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"ConditionCheck":{"TableName":"accounts","Key":{"id":{"S":"a"}},"ConditionExpression":"balance >= :amt","ExpressionAttributeValues":{":amt":{"N":"6"}}}}`+
		`]}`)
	wantErr(t, rec, typTransactionCanceled, "Transaction cancelled, please refer cancellation reasons for specific reasons [ConditionalCheckFailed]")
	m := bodyJSON(t, rec)
	equalJSON(t, m["CancellationReasons"], []any{
		map[string]any{"Code": "ConditionalCheckFailed", "Message": "The conditional request failed"},
	}, "CancellationReasons")
}

func TestTransactTwoEntriesOnOneItem(t *testing.T) {
	_, h, store := newOps(t)
	makeLedger(t, store)
	putItem(t, h, "accounts", `{"id":{"S":"a"},"balance":{"N":"5"}}`)

	rec := post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"Update":{"TableName":"accounts","Key":{"id":{"S":"a"}},"UpdateExpression":"SET balance = balance - :amt","ExpressionAttributeValues":{":amt":{"N":"1"}}}},`+
		`{"Update":{"TableName":"accounts","Key":{"id":{"S":"a"}},"UpdateExpression":"SET balance = balance + :amt","ExpressionAttributeValues":{":amt":{"N":"1"}}}}`+
		`]}`)
	wantErr(t, rec, typValidation, "Transaction request cannot include multiple operations on one item")
}

func TestTransactTooManyItems(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	var b strings.Builder
	b.WriteString(`{"TransactItems":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"Put":{"TableName":"test","Item":{"id":{"S":"i` + strconv.Itoa(i) + `"}}}}`)
	}
	b.WriteString(`]}`)

	rec := post(t, h, "TransactWriteItems", b.String())
	wantErr(t, rec, typValidation, "")
}

func TestTransactIdempotentRetry(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "test", storage.KeySchema{Hash: "id", HashType: attr.TypeS})

	body := `{"TransactItems":[{"Update":{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"ADD n :one","ExpressionAttributeValues":{":one":{"N":"1"}}}}],"ClientRequestToken":"tok-1"}`
	wantStatus(t, post(t, h, "TransactWriteItems", body), 200)

	// Same token and body: success without writing again.
	wantStatus(t, post(t, h, "TransactWriteItems", body), 200)
	rec := post(t, h, "GetItem", `{"TableName":"test","Key":{"id":{"S":"x"}}}`)
	m := wantStatus(t, rec, 200)
	item, _ := m["Item"].(map[string]any)
	n, _ := item["n"].(map[string]any)
	if got, _ := n["N"].(string); got != "1" {
		t.Errorf("n = %q, want 1 (idempotent retry wrote again)", got)
	}

	// Same token, different body: IdempotentParameterMismatchException.
	other := `{"TransactItems":[{"Update":{"TableName":"test","Key":{"id":{"S":"x"}},"UpdateExpression":"ADD n :one","ExpressionAttributeValues":{":one":{"N":"2"}}}}],"ClientRequestToken":"tok-1"}`
	rec = post(t, h, "TransactWriteItems", other)
	wantErr(t, rec, typIdempotentMismatch, "The request uses the same client token as a previous, but non-identical request.")
}

func TestTransactAllOldOnFailedEntry(t *testing.T) {
	_, h, store := newOps(t)
	makeLedger(t, store)
	putItem(t, h, "accounts", `{"id":{"S":"a"},"balance":{"N":"5"}}`)

	rec := post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"Put":{"TableName":"ledger","Item":{"id":{"S":"l1"}}}},`+
		`{"Update":{"TableName":"accounts","Key":{"id":{"S":"a"}},"UpdateExpression":"SET balance = balance - :amt","ConditionExpression":"balance >= :amt","ExpressionAttributeValues":{":amt":{"N":"100"}},"ReturnValuesOnConditionCheckFailure":"ALL_OLD"}}`+
		`]}`)
	wantErr(t, rec, typTransactionCanceled, "Transaction cancelled, please refer cancellation reasons for specific reasons [None, ConditionalCheckFailed]")
	m := bodyJSON(t, rec)
	equalJSON(t, m["CancellationReasons"], []any{
		map[string]any{"Code": "None"},
		map[string]any{
			"Code":    "ConditionalCheckFailed",
			"Message": "The conditional request failed",
			"Item": map[string]any{
				"id":      map[string]any{"S": "a"},
				"balance": map[string]any{"N": "5"},
			},
		},
	}, "CancellationReasons")
}

func TestTransactValidationErrorReason(t *testing.T) {
	_, h, store := newOps(t)
	makeLedger(t, store)
	putItem(t, h, "accounts", `{"id":{"S":"a"},"balance":{"N":"5"}}`)

	// ADD of a string operand onto a number is a wrong-type error found while
	// applying, reported as a ValidationError reason.
	rec := post(t, h, "TransactWriteItems", `{"TransactItems":[`+
		`{"Put":{"TableName":"ledger","Item":{"id":{"S":"l1"}}}},`+
		`{"Update":{"TableName":"accounts","Key":{"id":{"S":"a"}},"UpdateExpression":"ADD balance :s","ExpressionAttributeValues":{":s":{"S":"x"}}}}`+
		`]}`)
	wantErr(t, rec, typTransactionCanceled, "Transaction cancelled, please refer cancellation reasons for specific reasons [None, ValidationError]")
	m := bodyJSON(t, rec)
	equalJSON(t, m["CancellationReasons"], []any{
		map[string]any{"Code": "None"},
		map[string]any{"Code": "ValidationError", "Message": "Invalid UpdateExpression: Incorrect operand type for operator or function; operator: ADD, operand type: S"},
	}, "CancellationReasons")
}

func TestTransactConcurrentTransfersKeepTotal(t *testing.T) {
	_, h, store := newOps(t)
	makeLedger(t, store)
	putItem(t, h, "accounts", `{"id":{"S":"a"},"balance":{"N":"10"}}`)
	putItem(t, h, "accounts", `{"id":{"S":"b"},"balance":{"N":"0"}}`)

	const n = 20
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = post(t, h, "TransactWriteItems", transferBody("a", "b", "l"+strconv.Itoa(i), "1"))
		}(i)
	}
	wg.Wait()

	ok, failed := 0, 0
	for _, rec := range recs {
		switch rec.Code {
		case 200:
			ok++
		case 400:
			m := bodyJSON(t, rec)
			if m["__type"] != typTransactionCanceled {
				t.Errorf("__type = %v, want %s", m["__type"], typTransactionCanceled)
			}
			failed++
		default:
			t.Errorf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
	}
	if ok != 10 || failed != n-10 {
		t.Errorf("got %d success, %d failures; want 10, %d", ok, failed, n-10)
	}

	// The balance ran out at zero, so ten transfers succeeded; the total across
	// both accounts is unchanged.
	if got := balance(t, h, "a"); got != "0" {
		t.Errorf("a balance = %q, want 0", got)
	}
	if got := balance(t, h, "b"); got != "10" {
		t.Errorf("b balance = %q, want 10", got)
	}
}
