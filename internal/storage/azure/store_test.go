package azure

import "testing"

func TestContentRangeTotal(t *testing.T) {
	if total := contentRangeTotal("bytes 10-19/100", 10); total != 100 {
		t.Fatalf("total = %d", total)
	}
	if total := contentRangeTotal("", 10); total != 10 {
		t.Fatalf("fallback total = %d", total)
	}
}
