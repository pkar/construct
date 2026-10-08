package construct_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const payload = "#!/bin/sh\necho fixture-version\n"

type install struct {
	out      string
	requests string // every gh and curl invocation, one per line
	built    string // go invocations
	err      error
}

// runInstaller runs install.sh with PATH limited to basic utilities plus fake
// uname, gh, curl, go, and install, so nothing touches the network or the
// real home directory. mode selects fixture behaviour; modes starting with
// "no-gh" leave gh off PATH.
func runInstaller(t *testing.T, mode, system, machine string) install {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name string) {
		t.Helper()
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sh", "mktemp", "rm", "tr", "mkdir", "tar", "gzip", "cp", "cat"} {
		link(name)
	}
	if mode != "no-hash" {
		if _, err := exec.LookPath("sha256sum"); err == nil {
			link("sha256sum")
		} else {
			link("shasum")
		}
	}

	write("payload", payload)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
	if mode == "mismatch" {
		digest = strings.Repeat("0", 64)
	}
	asset := "construct-" + strings.ToLower(system) + "-" + map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[machine]
	manifest := digest + "  " + asset + "\n"
	switch mode {
	case "missing-entry":
		manifest = digest + "  unrelated\n"
	case "duplicate-entry":
		manifest += manifest
	}
	write("checksums", manifest)

	if err := os.Mkdir(filepath.Join(root, "source"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("source/go.mod", "module fixture\n")
	if out, err := exec.Command("tar", "-czf", filepath.Join(root, "source.tar.gz"), "-C", root, "source").CombinedOutput(); err != nil {
		t.Fatalf("fixture tar: %s: %v", out, err)
	}

	write("bin/uname", "#!/bin/sh\ncase \"$1\" in -s) echo \"$SYSTEM\";; -m) echo \"$MACHINE\";; esac\n")
	if !strings.HasPrefix(mode, "no-gh") {
		write("bin/gh", `#!/bin/sh
printf 'gh %s\n' "$*" >> "$ROOT/requests"
case "$1 $2" in
"auth status") [ "$MODE" != gh-logged-out ];;
"release view")
 [ "$MODE" != resolve-failure ] || exit 1
 if [ "$MODE" = invalid-tag ]; then echo main; else echo v1.2.3; fi;;
"release download")
 [ "$3" = v1.2.3 ] || exit 1
 pattern=''; out=''
 while [ "$#" -gt 0 ]; do
  case "$1" in --pattern) pattern=$2; shift 2;; --output) out=$2; shift 2;; *) shift;; esac
 done
 case "$pattern" in
  checksums.txt) cp "$ROOT/checksums" "$out";;
  construct-*) cp "$ROOT/payload" "$out";;
  *) exit 1;;
 esac;;
"api repos/pkar/construct/tarball/v1.2.3") cat "$ROOT/source.tar.gz";;
*) exit 1;;
esac
`)
	}
	write("bin/curl", `#!/bin/sh
printf 'curl %s\n' "$*" >> "$ROOT/requests"
out=''
while [ "$#" -gt 0 ]; do
 case "$1" in -o) out=$2; shift 2;; -w) shift 2;; -*) shift;; *) url=$1; shift;; esac
done
case "$url" in
 */releases/latest)
 [ "$MODE" != no-gh-unreachable ] || exit 22
 printf 'https://github.com/pkar/construct/releases/tag/v1.2.3';;
 */releases/download/v1.2.3/checksums.txt) cp "$ROOT/checksums" "$out";;
 */releases/download/v1.2.3/construct-*) cp "$ROOT/payload" "$out";;
 */archive/refs/tags/v1.2.3.tar.gz) cp "$ROOT/source.tar.gz" "$out";;
 *) exit 22;;
esac
`)
	write("bin/go", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$ROOT/built\"\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then cp \"$ROOT/payload\" \"$2\"; exit; fi; shift; done\nexit 1\n")
	write("bin/install", "#!/bin/sh\n[ \"$1 $2\" = '-m 0755' ] && [ \"$4\" = \"$CONSTRUCT_INSTALL_DIR/construct\" ] || exit 90\ncp \"$3\" \"$ROOT/result\"\n")

	cmd := exec.Command(filepath.Join(bin, "sh"), "install.sh")
	cmd.Env = []string{
		"PATH=" + bin, "HOME=" + root, "TMPDIR=" + root, "ROOT=" + root,
		"MODE=" + mode, "SYSTEM=" + system, "MACHINE=" + machine,
		"CONSTRUCT_INSTALL_DIR=" + filepath.Join(root, "dest"),
	}
	out, runErr := cmd.CombinedOutput()
	requests, _ := os.ReadFile(filepath.Join(root, "requests"))
	built, _ := os.ReadFile(filepath.Join(root, "built"))
	result, resultErr := os.ReadFile(filepath.Join(root, "result"))
	switch {
	case runErr != nil && resultErr == nil:
		t.Fatalf("installed despite failure: %s", out)
	case runErr == nil && string(result) != payload:
		t.Fatalf("succeeded without installing the payload: %s", out)
	}
	return install{out: string(out), requests: string(requests), built: string(built), err: runErr}
}

func TestInstallViaGh(t *testing.T) {
	r := runInstaller(t, "gh", "Linux", "x86_64")
	if r.err != nil {
		t.Fatalf("%v: %s", r.err, r.out)
	}
	if !strings.Contains(r.requests, "gh release download v1.2.3 --repo pkar/construct --pattern construct-linux-amd64") || strings.Contains(r.requests, "curl ") {
		t.Errorf("requests:\n%s", r.requests)
	}
	if !strings.Contains(r.out, "(v1.2.3)") {
		t.Errorf("output: %s", r.out)
	}
}

func TestInstallViaCurl(t *testing.T) {
	for _, mode := range []string{"no-gh", "gh-logged-out"} {
		t.Run(mode, func(t *testing.T) {
			r := runInstaller(t, mode, "Darwin", "arm64")
			if r.err != nil {
				t.Fatalf("%v: %s", r.err, r.out)
			}
			if !strings.Contains(r.requests, "curl -fsSL -o") || !strings.Contains(r.requests, "/releases/download/v1.2.3/construct-darwin-arm64") {
				t.Errorf("requests:\n%s", r.requests)
			}
			if strings.Contains(r.requests, "gh release") {
				t.Errorf("used gh while logged out:\n%s", r.requests)
			}
		})
	}
}

func TestInstallBuildsFromSource(t *testing.T) {
	for _, mode := range []string{"gh", "no-gh"} {
		t.Run(mode, func(t *testing.T) {
			r := runInstaller(t, mode, "Linux", "riscv64")
			if r.err != nil {
				t.Fatalf("%v: %s", r.err, r.out)
			}
			if !strings.Contains(r.built, "-X main.version=1.2.3") || !strings.Contains(r.built, "./cmd/construct") {
				t.Errorf("go invocations: %s", r.built)
			}
			if strings.Contains(r.requests, "release download") || strings.Contains(r.requests, "/releases/download/") {
				t.Errorf("downloaded a prebuilt binary for riscv64:\n%s", r.requests)
			}
		})
	}
}

func TestInstallFailures(t *testing.T) {
	for _, tc := range []struct {
		mode, system, machine, want string
	}{
		{"mismatch", "Linux", "aarch64", "checksum mismatch"},
		{"missing-entry", "Linux", "x86_64", "missing checksum"},
		{"duplicate-entry", "Linux", "x86_64", "ambiguous checksum"},
		{"no-hash", "Linux", "x86_64", "requires sha256sum or shasum"},
		{"invalid-tag", "Linux", "x86_64", "invalid release tag"},
		{"resolve-failure", "Linux", "x86_64", "could not resolve"},
		{"no-gh-unreachable", "Linux", "x86_64", "gh auth login"},
		{"gh", "FreeBSD", "amd64", "unsupported OS"},
	} {
		t.Run(tc.mode+"-"+tc.system, func(t *testing.T) {
			r := runInstaller(t, tc.mode, tc.system, tc.machine)
			if r.err == nil {
				t.Fatalf("succeeded, want failure: %s", r.out)
			}
			if !strings.Contains(r.out, tc.want) {
				t.Errorf("output %q, want %q", r.out, tc.want)
			}
		})
	}
}
