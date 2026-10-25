// Package text holds the small string helpers shared by the processing packages.
package textutil

// Truncate caps a diagnostic string at limit bytes.
func Truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
