// Package aghstrings contains utilities dealing with strings.
package aghstrings

import "strings"

// CloneSlice returns the exact copy of a.
func CloneSlice(a []string) (b []string) {
	return append(b, a...)
}

// CloneSliceOrEmpty returns the copy of a or empty strings slice if a is nil.
func CloneSliceOrEmpty(a []string) (b []string) {
	if a == nil {
		return []string{}
	}

	return CloneSlice(a)
}

// InSlice checks if string is in the slice of strings.
func InSlice(strs []string, str string) (ok bool) {
	for _, s := range strs {
		if s == str {
			return true
		}
	}

	return false
}

// SetSubtract subtracts b from a interpreted as sets.
func SetSubtract(a, b []string) (c []string) {
	// unit is an object to be used as value in set.
	type unit = struct{}

	cSet := make(map[string]unit)
	for _, k := range a {
		cSet[k] = unit{}
	}

	for _, k := range b {
		delete(cSet, k)
	}

	c = make([]string, len(cSet))
	i := 0
	for k := range cSet {
		c[i] = k
		i++
	}

	return c
}

// SplitNext splits string by a byte and returns the first chunk skipping empty
// ones.  Whitespaces are trimmed.
func SplitNext(s *string, sep rune) (chunk string) {
	if s == nil {
		return chunk
	}

	i := strings.IndexByte(*s, byte(sep))
	if i == -1 {
		chunk = *s
		*s = ""

		return strings.TrimSpace(chunk)
	}

	chunk = (*s)[:i]
	*s = (*s)[i+1:]
	var j int
	var r rune
	for j, r = range *s {
		if r != sep {
			break
		}
	}

	*s = (*s)[j:]

	return strings.TrimSpace(chunk)
}

// WriteToBuilder is a convenient wrapper for strings.(*Builder).WriteString
// that deals with multiple strings and ignores errors that are guaranteed to be
// nil.
func WriteToBuilder(b *strings.Builder, strs ...string) {
	// TODO(e.burkov): Recover from panic?
	for _, s := range strs {
		_, _ = b.WriteString(s)
	}
}