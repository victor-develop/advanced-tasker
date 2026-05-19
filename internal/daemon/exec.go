package daemon

import (
	"bytes"
	"os/exec"
	"strings"
)

// execProgram runs the given binary with args, feeding stdin, and
// returns (stdout, stderr, err). Centralized so tests can swap in.
func execProgram(exe, stdin string, args []string) (string, string, error) {
	cmd := exec.Command(exe, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var sout, serr bytes.Buffer
	cmd.Stdout = &sout
	cmd.Stderr = &serr
	err := cmd.Run()
	return sout.String(), serr.String(), err
}
