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

// UTF8Truncate limits value to limit bytes and appends suffix when it fits.
func UTF8Truncate(value string, limit int, suffix string) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	if limit < len(suffix) {
		return value[:UTF8Prefix(value, limit)]
	}
	return value[:UTF8Prefix(value, limit-len(suffix))] + suffix
}
