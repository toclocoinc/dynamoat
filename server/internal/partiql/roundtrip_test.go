package partiql

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// gen builds random valid PartiQL statements over the supported subset. Values
// are restricted to types that have a literal, so every statement round-trips
// through String and Parse.
type gen struct {
	r *rand.Rand
}

var genNames = []string{"a", "b", "c", "id", "sk", "val", "x", "attr"}

func (g *gen) name() string { return genNames[g.r.Intn(len(genNames))] }

func (g *gen) number() string { return strconv.Itoa(g.r.Intn(100)) }

func (g *gen) scalar() string {
	switch g.r.Intn(6) {
	case 0:
		return "'abc'"
	case 1:
		return strconv.Itoa(g.r.Intn(100) - 50)
	case 2:
		return strconv.Itoa(g.r.Intn(1000)) + "." + strconv.Itoa(g.r.Intn(9)+1)
	case 3:
		return "true"
	case 4:
		return "false"
	case 5:
		return "null"
	}
	return "'x'"
}

func (g *gen) path() string {
	p := g.name()
	if g.r.Intn(2) == 0 {
		p += "." + g.name()
	}
	if g.r.Intn(2) == 0 {
		p += "[" + strconv.Itoa(g.r.Intn(3)) + "]"
	}
	return p
}

func (g *gen) list() string {
	n := 1 + g.r.Intn(3)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = g.scalar()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (g *gen) mapp() string {
	n := 1 + g.r.Intn(2)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = g.name() + ": " + g.scalar()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (g *gen) set() string {
	if g.r.Intn(2) == 0 {
		n := 1 + g.r.Intn(2)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "'s" + strconv.Itoa(i) + "'"
		}
		return "<<" + strings.Join(parts, ", ") + ">>"
	}
	n := 1 + g.r.Intn(2)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = strconv.Itoa(i + 1)
	}
	return "<<" + strings.Join(parts, ", ") + ">>"
}

func (g *gen) primary() string {
	switch g.r.Intn(8) {
	case 0:
		return g.path() + " = " + g.scalar()
	case 1:
		op := []string{"<", "<=", ">", ">="}[g.r.Intn(4)]
		return g.path() + " " + op + " " + g.number()
	case 2:
		return g.path() + " BETWEEN " + g.number() + " AND " + g.number()
	case 3:
		return g.path() + " IN (" + g.scalar() + ", " + g.scalar() + ")"
	case 4:
		return g.path() + " IN [" + g.scalar() + ", " + g.scalar() + "]"
	case 5:
		is := []string{"MISSING", "NOT MISSING", "NULL", "NOT NULL"}[g.r.Intn(4)]
		return g.path() + " IS " + is
	case 6:
		fn := []string{"begins_with", "contains"}[g.r.Intn(2)]
		return fn + "(" + g.path() + ", " + g.scalar() + ")"
	case 7:
		op := []string{"=", ">", "<"}[g.r.Intn(3)]
		return "size(" + g.path() + ") " + op + " " + g.number()
	}
	return g.path() + " = " + g.scalar()
}

func (g *gen) cond(depth int) string {
	if depth <= 0 {
		return g.primary()
	}
	switch g.r.Intn(7) {
	case 0:
		return g.primary()
	case 1:
		return "NOT " + g.cond(depth-1)
	case 2:
		return "(" + g.cond(depth-1) + ")"
	case 3:
		return g.cond(depth-1) + " AND " + g.cond(depth-1)
	case 4:
		return g.cond(depth-1) + " OR " + g.cond(depth-1)
	case 5:
		return "attribute_type(" + g.path() + ", 'S')"
	case 6:
		return g.primary()
	}
	return g.primary()
}

func (g *gen) keyCond() string {
	s := "id = " + g.number()
	if g.r.Intn(2) == 0 {
		s += " AND " + g.cond(2)
	}
	return s
}

func (g *gen) setAction() string {
	p := g.path()
	switch g.r.Intn(5) {
	case 0:
		return p + " = " + g.scalar()
	case 1:
		return p + " = " + g.path() + " + " + g.number()
	case 2:
		return p + " = list_append(" + g.path() + ", [" + g.scalar() + ", " + g.scalar() + "])"
	case 3:
		return p + " = set_add(" + p + ", <<1, 2>>)"
	case 4:
		return p + " = set_delete(" + p + ", <<3>>)"
	}
	return p + " = " + g.scalar()
}

func (g *gen) statement() string {
	tbl := "t"
	switch g.r.Intn(5) {
	case 0:
		proj := "*"
		if g.r.Intn(2) == 0 {
			proj = g.path() + ", " + g.path()
		}
		s := "SELECT " + proj + " FROM " + tbl
		if g.r.Intn(3) == 0 {
			s += " WHERE " + g.cond(2)
		}
		return s
	case 1:
		return "INSERT INTO " + tbl + " VALUE " + g.mapp()
	case 2:
		s := "UPDATE " + tbl + " SET " + g.setAction()
		if g.r.Intn(2) == 0 {
			s += " REMOVE " + g.path()
		}
		return s + " WHERE " + g.keyCond()
	case 3:
		return "DELETE FROM " + tbl + " WHERE " + g.keyCond()
	case 4:
		return "EXISTS(SELECT * FROM " + tbl + " WHERE " + g.cond(2) + ")"
	}
	return "SELECT * FROM " + tbl
}

func TestRoundTrip(t *testing.T) {
	g := &gen{r: rand.New(rand.NewSource(1))}
	for i := 0; i < 200; i++ {
		src := g.statement()
		p, err := Parse(src, nil)
		if err != nil {
			t.Fatalf("iteration %d: Parse(%q) error: %v", i, src, err)
		}
		s := p.String()
		p2, err := Parse(s, nil)
		if err != nil {
			t.Fatalf("iteration %d: Parse(String(%q)) = %q error: %v", i, src, s, err)
		}
		if got := p2.String(); got != s {
			t.Fatalf("iteration %d: round trip mismatch:\n src=%q\n s=%q\n again=%q", i, src, s, got)
		}
	}
}
