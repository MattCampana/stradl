//go:build darwin

package profiles

import (
	"context"
	"encoding/binary"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Instances lists the Claude app's running instances: each main process (an
// exact name match, so helpers and the claude CLI don't count; -a so it
// counts when it started this process) and the data folder it was started
// with, mainDir when none.
func Instances(mainDir string) map[int]string {
	inst, _ := InstancesChecked(mainDir)
	return inst
}

// InstancesChecked is Instances, with ok false when the processes couldn't
// be listed.
func InstancesChecked(mainDir string) (inst map[int]string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-a", "-x", "Claude").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, true // pgrep exits 1 when nothing matches
	}
	if err != nil {
		return nil, false
	}
	inst = make(map[int]string)
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid <= 0 {
			continue
		}
		args, err := procArgs(pid)
		if err != nil {
			continue // gone meanwhile, or not ours
		}
		inst[pid] = DataDir(args, mainDir)
	}
	return inst, true
}

// procArgs returns a process's arguments (kern.procargs2: argc, the
// executable path, padding, then argc NUL-terminated arguments).
func procArgs(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(raw) < 4 {
		return nil, unix.EINVAL
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	i := 0
	for i < len(rest) && rest[i] != 0 { // the executable path
		i++
	}
	for i < len(rest) && rest[i] == 0 {
		i++
	}
	var args []string
	for len(args) < argc && i < len(rest) {
		j := i
		for j < len(rest) && rest[j] != 0 {
			j++
		}
		args = append(args, string(rest[i:j]))
		i = j + 1
	}
	return args, nil
}
