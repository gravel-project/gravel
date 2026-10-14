package deploy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Postgres 17 comes from gravel's mirror on ghcr.io, pinned to one digest everywhere (#233).
// Dependabot bumps the Containerfile alone; this fails until the others follow, so a host, CI and
// the release never run different Postgres builds, and nothing pulls from ECR Public, which caps
// anonymous pulls.
func TestPostgresPinsAgree(t *testing.T) {
	const image = "ghcr.io/gravel-project/mirror/postgres:17"
	pinned := regexp.MustCompile(regexp.QuoteMeta(image) + `@(sha256:[0-9a-f]{64})`)
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := pinned.FindStringSubmatch(read("postgres/Containerfile"))
	if m == nil {
		t.Fatalf("postgres/Containerfile does not build on %s@sha256:…", image)
	}
	digest := m[1]
	for _, path := range []string{"quadlet/gravel-postgres.container", "observability/check.sh", "../.github/workflows/ci.yml"} {
		text := read(path)
		if strings.Contains(text, "public.ecr.aws") {
			t.Errorf("%s pulls from public.ecr.aws; use %s", path, image)
		}
		got := pinned.FindAllStringSubmatch(text, -1)
		if len(got) == 0 {
			t.Errorf("%s does not pin %s@sha256:…", path, image)
		}
		for _, g := range got {
			if g[1] != digest {
				t.Errorf("%s pins %s, the Containerfile %s: move them together", path, g[1], digest)
			}
		}
	}
}
