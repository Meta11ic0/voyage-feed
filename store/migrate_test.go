package store

import "testing"

func TestSplitSQL(t *testing.T) {
	parts := splitSQL("CREATE TABLE a (id int);\n\nCREATE TABLE b (id int);\n")
	if len(parts) != 2 {
		t.Fatalf("statements %d", len(parts))
	}
}
