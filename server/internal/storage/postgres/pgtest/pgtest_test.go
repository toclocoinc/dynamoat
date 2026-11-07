package pgtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMain(m *testing.M) { Main(m) }

// Two tests get two separate, working databases on a supported version.
func TestDBIsFreshAndSupported(t *testing.T) {
	ctx := context.Background()
	var names []string
	for i := 0; i < 2; i++ {
		c, err := pgx.Connect(ctx, DB(t))
		if err != nil {
			t.Fatal(err)
		}
		var name string
		var ver int
		if err := c.QueryRow(ctx, "SELECT current_database(), current_setting('server_version_num')::int").Scan(&name, &ver); err != nil {
			t.Fatal(err)
		}
		if ver < 150000 {
			t.Errorf("server_version_num %d, want 15 or newer", ver)
		}
		var n int
		if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'").Scan(&n); err != nil || n != 0 {
			t.Errorf("new database has %d tables (err %v)", n, err)
		}
		names = append(names, name)
		_ = c.Close(ctx)
	}
	if names[0] == names[1] {
		t.Errorf("both calls returned database %s", names[0])
	}
}
