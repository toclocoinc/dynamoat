package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres/pgtest"
)

// TestQueryUsesIndexScan checks, on a table of 20,000 rows, that the base
// Query and the GSI Query are served by index scans: items_pkey for the base
// partition, index_entries_pkey (joined to items_pkey) for the GSI.
func TestQueryUsesIndexScan(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, pgtest.DB(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:     "big",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "name": attr.TypeS},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
		GSIs: []storage.Index{{
			Name:       "byName",
			Key:        storage.KeySchema{Hash: "name", HashType: attr.TypeS},
			Projection: storage.ProjectAll,
		}},
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	const n = 20000
	items := make([][]any, 0, n)
	entries := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%05d", i)
		name := fmt.Sprintf("name%05d", i)
		pk := []byte(id)
		ipk := []byte(name)
		itemJSON, err := json.Marshal(attr.Item{"id": sv(id), "name": sv(name)})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, []any{tb.ID, attr.KeyHash(pk), pk, []byte{}, itemJSON, nil})
		entries = append(entries, []any{tb.ID, tb.GSIs[0].ID, attr.KeyHash(ipk), ipk, []byte{}, attr.KeyHash(pk), pk, []byte{}})
	}
	copyRows := func(table string, cols []string, rows [][]any) {
		t.Helper()
		if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows)); err != nil {
			t.Fatalf("copy %s: %v", table, err)
		}
	}
	copyRows("items", []string{"table_id", "pk_hash", "pk", "sk", "item", "ttl_at"}, items)
	copyRows("index_entries", []string{"table_id", "index_id", "ipk_hash", "ipk", "isk", "pk_hash", "pk", "sk"}, entries)

	if _, err := s.pool.Exec(ctx, `ANALYZE items; ANALYZE index_entries;`); err != nil {
		t.Fatal(err)
	}

	baseSQL, baseArgs := baseQuerySQL(tb, mustEncode(sv("id00042")), keyBounds{}, true)
	explainUsesIndexScan(t, s, baseSQL, baseArgs, "items_pkey")

	gsiSQL, gsiArgs := indexQuerySQL(tb, &tb.GSIs[0], mustEncode(sv("name00042")), keyBounds{}, true, false, queryCursor{})
	explainUsesIndexScan(t, s, gsiSQL, gsiArgs, "index_entries_pkey", "items_pkey")
}

func explainUsesIndexScan(t *testing.T, s *Store, sql string, args []any, wantIndexes ...string) {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), "EXPLAIN (FORMAT TEXT) "+inlineArgs(sql, args))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, idx := range wantIndexes {
		if !strings.Contains(plan.String(), "Index Scan using "+idx) {
			t.Fatalf("plan does not use an index scan on %s:\n%s", idx, plan.String())
		}
	}
}

var inlineArgRE = regexp.MustCompile(`\$(\d+)`)

// inlineArgs renders a parameterised SQL string with its arguments as literals,
// so it can be run under EXPLAIN without a prepared statement.
func inlineArgs(sql string, args []any) string {
	return inlineArgRE.ReplaceAllStringFunc(sql, func(m string) string {
		n, _ := strconv.Atoi(m[1:])
		return sqlLiteral(args[n-1])
	})
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case []byte:
		return `'\x` + hex.EncodeToString(x) + `'`
	default:
		return fmt.Sprintf("%v", v)
	}
}
