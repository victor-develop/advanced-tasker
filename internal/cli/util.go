package cli

import "os"

// writeFile is a thin atomic-ish wrapper used by verbs that overwrite an
// existing tracked file (goal.md). It is intentionally not atomic across
// fsync; the surrounding git commit is the durability guarantee.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// readFile returns the file contents or "" on any error. Callers use it
// for best-effort display only.
func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
