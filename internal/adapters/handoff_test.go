package adapters

import "testing"

func TestSplitHandoffReceipt(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantOK   bool
		wantID   string
		wantRest string
	}{
		{
			name:   "complete root package",
			input:  "[[TERMINUS_HANDOFF:abc-123:BEGIN]]\nhistory\n[[TERMINUS_HANDOFF:abc-123:END]]",
			wantOK: true,
			wantID: "abc-123",
		},
		{
			name:     "complete package then user request",
			input:    "[[TERMINUS_HANDOFF:abc-123:BEGIN]]\nhistory\n[[TERMINUS_HANDOFF:abc-123:END]]\n\ncontinue please",
			wantOK:   true,
			wantID:   "abc-123",
			wantRest: "continue please",
		},
		{
			name:   "marker mentioned in prose is not a receipt",
			input:  "The old message said [[TERMINUS_HANDOFF:abc-123:BEGIN]] and later [[TERMINUS_HANDOFF:abc-123:END]].",
			wantOK: false,
		},
		{
			name:   "unpaired begin",
			input:  "[[TERMINUS_HANDOFF:abc-123:BEGIN]]\nhistory",
			wantOK: false,
		},
		{
			name:   "mismatched end",
			input:  "[[TERMINUS_HANDOFF:abc-123:BEGIN]]\nhistory\n[[TERMINUS_HANDOFF:other:END]]",
			wantOK: false,
		},
		{
			name:   "leading text prevents root match",
			input:  "prefix [[TERMINUS_HANDOFF:abc-123:BEGIN]]\nhistory\n[[TERMINUS_HANDOFF:abc-123:END]]",
			wantOK: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt, rest, packetID, ok := SplitHandoffReceipt(test.input)
			if ok != test.wantOK {
				t.Fatalf("ok = %v, want %v", ok, test.wantOK)
			}
			if !ok {
				return
			}
			if packetID != test.wantID {
				t.Fatalf("packet ID = %q, want %q", packetID, test.wantID)
			}
			if rest != test.wantRest {
				t.Fatalf("rest = %q, want %q", rest, test.wantRest)
			}
			if receipt != test.input[:len(receipt)] {
				t.Fatal("receipt is not a prefix of the original message")
			}
		})
	}
}

func TestIsHandoffReceiptRequiresNoTrailingRequest(t *testing.T) {
	if !IsHandoffReceipt("[[TERMINUS_HANDOFF:p1:BEGIN]]\nx\n[[TERMINUS_HANDOFF:p1:END]]") {
		t.Fatal("complete package should be a receipt")
	}
	if IsHandoffReceipt("[[TERMINUS_HANDOFF:p1:BEGIN]]\nx\n[[TERMINUS_HANDOFF:p1:END]]\nreal user request") {
		t.Fatal("package with a following request is not a pure receipt")
	}
}
