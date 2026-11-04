package githooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookChecksOnlyStagedFiles(t *testing.T) {
	hook, err := os.ReadFile("pre-commit")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		externalDB, fail bool
	}{
		{name: "local setup"}, {name: "explicit database", externalDB: true}, {name: "failed checks", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{}
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "GIT_") && !strings.HasPrefix(e, "MEDIAD_TEST_DSN=") {
					env = append(env, e)
				}
			}
			env = append(env, "TMPDIR="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			if tc.externalDB {
				env = append(env, "MEDIAD_TEST_DSN=explicit-test-database")
			}
			if tc.fail {
				env = append(env, "FAIL_CHECKS=1")
			}
			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command(args[0], args[1:]...)
				cmd.Dir = dir
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %v\n%s", args, err, out)
				}
				return string(out)
			}
			run("git", "init", "-q")
			write("tracked.go", "staged\n", 0600)
			write("pre-commit", string(hook), 0700)
			run("git", "add", "tracked.go", "pre-commit")
			write("tracked.go", "unstaged\n", 0600)
			write("untracked.go", "untracked\n", 0600)
			write(".env", "private-settings\n", 0600)
			// The real Git index/snapshot are used; make is replaced to inspect what it sees.
			write("make", `#!/bin/sh
set -eu
if [ "$1" = test-db-setup ]; then
 test "$(cat tracked.go)" = unstaged
 printf setup > setup-ran
 exit 0
fi
test "$1" = check-commit
test "$(cat tracked.go)" = staged
test ! -e untracked.go
test "$(cat .env)" = private-settings
test ! -e setup-ran
test -z "${GIT_DIR:-}"
printf '%s' "$PWD" > "$TMPDIR/snapshot-path"
if [ "${FAIL_CHECKS:-}" = 1 ]; then exit 42; fi
`, 0700)
			before := run("git", "ls-files", "--stage")
			cmd := exec.Command("sh", "./pre-commit")
			cmd.Dir = dir
			// Git supplies GIT_INDEX_FILE to hooks during some commit modes.
			cmd.Env = append(env, "GIT_INDEX_FILE="+filepath.Join(dir, ".git", "index"))
			out, hookErr := cmd.CombinedOutput()
			if (hookErr != nil) != tc.fail {
				t.Fatalf("hook error = %v, output: %s", hookErr, out)
			}
			if tc.fail {
				exit, ok := hookErr.(*exec.ExitError)
				if !ok || exit.ExitCode() != 42 {
					t.Fatalf("lost check failure: %v", hookErr)
				}
			}
			if after := run("git", "ls-files", "--stage"); after != before {
				t.Fatal("hook changed the index")
			}
			if got := run("git", "show", ":tracked.go"); got != "staged\n" {
				t.Fatal("unstaged content entered the index")
			}
			b, _ := os.ReadFile(filepath.Join(dir, "tracked.go"))
			if string(b) != "unstaged\n" {
				t.Fatal("hook changed the working file")
			}
			_, setupErr := os.Stat(filepath.Join(dir, "setup-ran"))
			if (setupErr == nil) == tc.externalDB {
				t.Fatal("wrong database setup policy")
			}
			snapshot, err := os.ReadFile(filepath.Join(dir, "snapshot-path"))
			if err != nil {
				t.Fatalf("snapshot was not checked: %v\n%s", err, out)
			}
			if _, err := os.Stat(string(snapshot)); !os.IsNotExist(err) {
				t.Fatal("temporary snapshot was not cleaned up")
			}
		})
	}
}

func TestTestTargetsRunEveryCheck(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"test", "test-db", "check", "check-ci", "check-commit"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", target, explicit), func(t *testing.T) {
				dir := t.TempDir()
				settings := "MEDIAD_POSTGRES_DSN=local-dsn\nMEDIAD_TEST_CHROME=" + filepath.Join(dir, "chrome") + "\n"
				if target != "check-commit" {
					settings += "MEDIAD_TEST_QBIT_URL=http://fixture.invalid\n"
				}
				for name, body := range map[string]string{
					"Makefile": string(makefile), ".env": settings,
					"chrome": "#!/bin/sh\nexit 0\n", "node": "#!/bin/sh\nexit 0\n",
					"gofmt":         "#!/bin/sh\necho fmt-check >> checks\n",
					"workflowcheck": "#!/bin/sh\nexit 0\n",
					"go": `#!/bin/sh
printf '%s\n' "$*" >> checks
if [ "$1" = test ]; then
 printf 'DSN=%s REPLAY=%s CHROME=%s\n' "$MEDIAD_TEST_DSN" "$MEDIAD_REQUIRE_REPLAY" "$MEDIAD_TEST_CHROME"
fi
`,
				} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.Command("make", target, "WORKFLOWCHECK="+filepath.Join(dir, "workflowcheck"))
				cmd.Dir = dir
				for _, e := range os.Environ() {
					if !strings.HasPrefix(e, "MEDIAD_") && !strings.HasPrefix(e, "MAKEFLAGS=") {
						cmd.Env = append(cmd.Env, e)
					}
				}
				cmd.Env = append(cmd.Env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				want := "local-dsn"
				if explicit {
					want = "explicit-dsn"
					cmd.Env = append(cmd.Env, "MEDIAD_TEST_DSN="+want)
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("make: %v\n%s", err, out)
				}
				if !strings.Contains(string(out), "DSN="+want+" REPLAY=1 CHROME="+filepath.Join(dir, "chrome")) {
					t.Fatalf("wrong test settings: %s", out)
				}
				checks, err := os.ReadFile(filepath.Join(dir, "checks"))
				if err != nil {
					t.Fatal(err)
				}
				for _, check := range []string{"fmt-check", "mod verify", "vet ./...", "vet -vettool ", "build -ldflags", "test ./... -count=1 -race -timeout 10m"} {
					if strings.Count(string(checks), check) != 1 {
						t.Fatalf("expected exactly one %q check, got:\n%s", check, checks)
					}
				}
			})
		}
	}
}

func TestTestTargetRejectsMissingSettings(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"MEDIAD_POSTGRES_DSN", "MEDIAD_TEST_QBIT_URL", "MEDIAD_TEST_CHROME"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			settings := map[string]string{"MEDIAD_POSTGRES_DSN": "local-dsn", "MEDIAD_TEST_QBIT_URL": "http://fixture.invalid", "MEDIAD_TEST_CHROME": filepath.Join(dir, "missing-chrome")}
			settings[missing] = ""
			var env strings.Builder
			for key, value := range settings {
				fmt.Fprintf(&env, "%s=%s\n", key, value)
			}
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env.String()), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("make", "test", "MEDIAD_TEST_CHROME="+settings["MEDIAD_TEST_CHROME"])
			cmd.Dir = dir
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "MEDIAD_") {
					cmd.Env = append(cmd.Env, e)
				}
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("missing %s passed: %s", missing, out)
			}
			if !strings.Contains(string(out), missing) {
				t.Fatalf("missing %s not explained: %s", missing, out)
			}
		})
	}
}
