package github

import (
	"errors"
	"os"
)

// readDirNoFail returns os.ReadDir(dir), but treats ErrNotExist as an empty
// result instead of an error.  Useful where the parent state/threads
// directory may simply not exist yet (no PRs have ever been tracked).
func readDirNoFail(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err == nil {
		return entries, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return nil, err
}
