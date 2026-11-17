package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoShippedUnitLineReadsAsSystemctlCatsHeader keeps every systemd
// unit the package ships from packaging/ free of lines that open with
// "# /". systemctl cat prints each file it shows under a "# <path>"
// line, and docs/upgrading.md tells the operator to read the output by
// those lines, so a comment that opens the same way reads as one more
// file in play.
func TestNoShippedUnitLineReadsAsSystemctlCatsHeader(t *testing.T) {
	var units []string
	for _, pattern := range []string{"*.service", "*.timer", "*.path"} {
		matches, err := filepath.Glob(filepath.Join("../../packaging", pattern))
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, matches...)
	}
	if len(units) == 0 {
		t.Fatal("found no .service, .timer or .path file in ../../packaging")
	}
	for _, unit := range units {
		data, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "# /") {
				t.Errorf("%s:%d opens with \"# /\", the shape of the \"# <path>\" line systemctl cat prints above each file it shows; docs/upgrading.md tells the operator to read that output by those lines, so this one reads as a file in play. Rewrap the comment, or write the path in backticks, so no line opens with one: %q",
					unit, i+1, line)
			}
		}
	}
}
