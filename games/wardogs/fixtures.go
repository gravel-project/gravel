package wardogs

import (
	"cmp"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// recorded is every recorded build (testdata/<build>/, games/wardogs/README.md), embedded so a
// binary can serve one: the demo server (wardogsim) answers as the newest build does. The files
// stay the single source; the contract tests read the same directory from disk.
//
//go:embed testdata
var recorded embed.FS

// Recordings are the recorded builds' directory names, oldest first: by changelist for the
// recorder's "CL-<changelist>" names, by name otherwise.
func Recordings() []string {
	entries, err := fs.ReadDir(recorded, "testdata")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(recorded, path.Join("testdata", e.Name(), "v1", "capabilities.json")); err == nil {
			out = append(out, e.Name())
		}
	}
	slices.SortFunc(out, compareBuilds)
	return out
}

func compareBuilds(a, b string) int {
	na, errA := strconv.Atoi(strings.TrimPrefix(a, "CL-"))
	nb, errB := strconv.Atoi(strings.TrimPrefix(b, "CL-"))
	if errA == nil && errB == nil && na != nb {
		return cmp.Compare(na, nb)
	}
	return strings.Compare(a, b)
}

// Recording is one recorded build's files, rooted at the build's directory (v1/capabilities.json,
// v1/status.json, …), as embedded in the binary.
func Recording(build string) (fs.FS, error) {
	if !slices.Contains(Recordings(), build) {
		return nil, fmt.Errorf("wardogs: no recording of build %q", build)
	}
	return fs.Sub(recorded, path.Join("testdata", build))
}

// NewestRecording is the newest recorded build's name and files.
func NewestRecording() (string, fs.FS, error) {
	builds := Recordings()
	if len(builds) == 0 {
		return "", nil, fmt.Errorf("wardogs: no build is recorded")
	}
	build := builds[len(builds)-1]
	files, err := Recording(build)
	return build, files, err
}
