package adapters

import (
	"regexp"
	"strings"
)

var handoffBeginPattern = regexp.MustCompile(`^\[\[BATON_HANDOFF:([A-Za-z0-9][A-Za-z0-9._-]*):BEGIN\]\]`)

// SplitHandoffReceipt recognizes a complete handoff marker only when the BEGIN
// marker is at the root of text and its matching END marker closes that package.
// Text after the closing marker is returned separately so a target's actual new
// user request is never lost.
//
// A handoff marker mentioned inside ordinary prose, quoted history, or a user
// message with only one half of the pair is not a receipt.
func SplitHandoffReceipt(text string) (receipt, rest, packetID string, ok bool) {
	match := handoffBeginPattern.FindStringSubmatchIndex(text)
	if match == nil || match[0] != 0 {
		return "", "", "", false
	}
	packetID = text[match[2]:match[3]]
	endMarker := "[[BATON_HANDOFF:" + packetID + ":END]]"
	endOffset := strings.Index(text[match[1]:], endMarker)
	if endOffset < 0 {
		return "", "", "", false
	}
	end := match[1] + endOffset + len(endMarker)
	return text[:end], strings.TrimSpace(text[end:]), packetID, true
}

// IsHandoffReceipt reports whether text is exactly one complete root-level
// handoff package, with no appended user request.
func IsHandoffReceipt(text string) bool {
	_, rest, _, ok := SplitHandoffReceipt(text)
	return ok && rest == ""
}

func visibleUserText(text string) string {
	if _, rest, _, ok := SplitHandoffReceipt(text); ok {
		return rest
	}
	return text
}
