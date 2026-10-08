package postgres

import "testing"

func TestPgx5URL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"postgres://u:p@h:5432/db?sslmode=disable", "pgx5://u:p@h:5432/db?sslmode=disable", false},
		{"postgresql://u:p@h/db", "pgx5://u:p@h/db", false},
		{"mysql://u:p@h/db", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := pgx5URL(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("pgx5URL(%q) = %q, %v; want %q, erro=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}
