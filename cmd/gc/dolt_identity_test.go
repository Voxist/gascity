package main

import (
	"path/filepath"
	"testing"
)

func TestFirstManagedDoltCSVValue(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want string
	}{
		"header + value":   {"@@datadir\n/var/db/dolt\n", "/var/db/dolt"},
		"quoted value":     {"@@datadir\n\"/var/db/dolt\"\n", "/var/db/dolt"},
		"crlf":             {"@@datadir\r\n/var/db/dolt\r\n", "/var/db/dolt"},
		"trailing blanks":  {"@@datadir\n/var/db/dolt\n\n", "/var/db/dolt"},
		"header only":      {"@@datadir\n", ""},
		"empty":            {"", ""},
		"whitespace value": {"@@datadir\n   /var/db/dolt   \n", "/var/db/dolt"},
	} {
		if got := firstManagedDoltCSVValue(tc.out); got != tc.want {
			t.Errorf("%s: firstManagedDoltCSVValue(%q) = %q, want %q", name, tc.out, got, tc.want)
		}
	}
}

func TestDataDirIsMismatch(t *testing.T) {
	expected := filepath.Join(t.TempDir(), ".beads", "dolt")
	for name, tc := range map[string]struct {
		serving string
		want    bool
	}{
		"same path":                           {expected, false},
		"same with whitespace":                {"  " + expected + "  ", false},
		"different path":                      {"/some/other/dolt", true},
		"serving empty":                       {"", false}, // cannot conclude → fail open
		"expected matches via trailing slash": {expected + "/", false},
	} {
		if got := dataDirIsMismatch(tc.serving, expected); got != tc.want {
			t.Errorf("%s: dataDirIsMismatch(%q, %q) = %v, want %v", name, tc.serving, expected, got, tc.want)
		}
	}
	// Either side empty must fail open regardless of the other.
	if dataDirIsMismatch("/a/b", "") {
		t.Errorf("dataDirIsMismatch with empty expected = true, want false (fail open)")
	}
}
