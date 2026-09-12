package textutil

import "unicode/utf8"

// UTF8Prefix returns the largest byte index at or below limit that ends on a
// UTF-8 boundary.
func UTF8Prefix(value string, limit int) int {
	end := 0
	for end < len(value) {
		_, size := utf8.DecodeRuneInString(value[end:])
		if end+size > limit {
			break
		}
		end += size
	}
	return end
}
