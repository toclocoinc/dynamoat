package expr

import (
	"errors"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func updateEnv() *Env {
	return &Env{
		Names:  map[string]string{"#n": "name", "#s": "status", "#c": "counter"},
		Values: map[string]bool{":v": true, ":w": true, ":one": true, ":inc": true, ":zero": true, ":more": true, ":t": true, ":name": true, ":now": true, ":s": true},
	}
}

func TestParseUpdateValid(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"set value", "SET a = :v", "SET a = :v"},
		{"placeholder and path", "SET #n = :v, b = c", "SET name = :v, b = c"},
		{"increment", "SET n = n + :one", "SET n = n + :one"},
		{"subtract", "SET n = n - :one", "SET n = n - :one"},
		{"if_not_exists arithmetic", "SET n = if_not_exists(n, :zero) + :inc", "SET n = if_not_exists(n, :zero) + :inc"},
		{"list_append", "SET l = list_append(l, :more)", "SET l = list_append(l, :more)"},
		{"remove", "REMOVE a, b.c[2]", "REMOVE a, b.c[2]"},
		{"add", "ADD #c :one, tags :t", "ADD counter :one, tags :t"},
		{"delete", "DELETE tags :t", "DELETE tags :t"},
		{"all clauses reordered", "DELETE d :t ADD c :one REMOVE r SET a = :v", "SET a = :v REMOVE r ADD c :one DELETE d :t"},
		{"lower case", "set a = :v remove b add c :one delete d :t", "SET a = :v REMOVE b ADD c :one DELETE d :t"},
		{"name and timestamp", "SET #name = :name, updated = :now ADD version :one", "SET name = :name, updated = :now ADD version :one"},
		{"status and remove", "SET #s = :s REMOVE assignee", "SET status = :s REMOVE assignee"},
		{"siblings do not overlap", "SET a.b = :v, a.c = :w, l[0] = :v REMOVE l[1]", "SET a.b = :v, a.c = :w, l[0] = :v REMOVE l[1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "name and timestamp" {
				e := updateEnv()
				e.Names["#name"] = "name"
				u, err := ParseUpdate(tc.in, e)
				if err != nil || StringUpdate(u) != tc.want {
					t.Fatalf("got %q, %v", StringUpdate(u), err)
				}
				return
			}
			u, err := ParseUpdate(tc.in, updateEnv())
			if err != nil {
				t.Fatal(err)
			}
			if got := StringUpdate(u); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestParseUpdateErrors(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty", "  ", "Invalid UpdateExpression: The expression can not be empty;"},
		{"set twice", "SET a = :v SET b = :w", `Invalid UpdateExpression: The "SET" section can only be used once in an update expression;`},
		{"remove twice", "REMOVE a REMOVE b", `Invalid UpdateExpression: The "REMOVE" section can only be used once in an update expression;`},
		{"same path", "SET a = :v, a = :w", "Invalid UpdateExpression: Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a], path two: [a]"},
		{"prefix across clauses", "SET a = :v REMOVE a.b", "Invalid UpdateExpression: Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a], path two: [a, b]"},
		{"index prefix", "REMOVE a[0], a", "Invalid UpdateExpression: Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a, [0]], path two: [a]"},
		{"chain", "SET a = b + c + d", `Invalid UpdateExpression: Syntax error; token: "+", near: "c +"`},
		{"size", "SET a = size(b)", "Invalid UpdateExpression: Invalid function name; function: size"},
		{"add no value", "ADD a", `Invalid UpdateExpression: Syntax error; token: "<EOF>", near: "a"`},
		{"bare set", "SET", `Invalid UpdateExpression: Syntax error; token: "<EOF>", near: "SET"`},
		{"undefined value", "SET a = :zzz", "Invalid UpdateExpression: An expression attribute value used in expression is not defined; attribute value: :zzz"},
		{"undefined name", "SET #x = :v", "Invalid UpdateExpression: An expression attribute name used in the document path is not defined; attribute name: #x"},
		{"reserved", "SET name = :v", "Invalid UpdateExpression: Attribute name is a reserved keyword; reserved keyword: name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUpdate(tc.in, updateEnv())
			var de *ddberr.Error
			if !errors.As(err, &de) || de.Message != tc.want {
				t.Fatalf("got %v\nwant %s", err, tc.want)
			}
		})
	}
}
