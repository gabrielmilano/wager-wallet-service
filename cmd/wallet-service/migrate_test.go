package main

import "testing"

func TestParseMigrateArgs(t *testing.T) {
	tests := []struct {
		args    []string
		want    migrateCmd
		wantErr bool
	}{
		{[]string{"up"}, migrateCmd{name: "up"}, false},
		{[]string{"version"}, migrateCmd{name: "version"}, false},
		{[]string{"down"}, migrateCmd{name: "down", n: 1}, false},
		{[]string{"down", "3"}, migrateCmd{name: "down", n: 3}, false},
		{[]string{"force", "5"}, migrateCmd{name: "force", n: 5}, false},
		{[]string{"force", "-1"}, migrateCmd{name: "force", n: -1}, false},
		{nil, migrateCmd{}, true},
		{[]string{"up", "2"}, migrateCmd{}, true},
		{[]string{"down", "0"}, migrateCmd{}, true},
		{[]string{"down", "x"}, migrateCmd{}, true},
		{[]string{"down", "1", "2"}, migrateCmd{}, true},
		{[]string{"force"}, migrateCmd{}, true},
		{[]string{"force", "-2"}, migrateCmd{}, true},
		{[]string{"drop"}, migrateCmd{}, true},
	}
	for _, tt := range tests {
		got, err := parseMigrateArgs(tt.args)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseMigrateArgs(%q) = %+v, %v; want %+v, erro=%v", tt.args, got, err, tt.want, tt.wantErr)
		}
	}
}
