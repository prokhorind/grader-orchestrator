package classroom

import "strings"

// Sanitize replaces characters that are problematic in file/directory names.
func Sanitize(s string) string {
	return strings.NewReplacer(" ", "_", "/", "-", "\\", "-").Replace(s)
}
