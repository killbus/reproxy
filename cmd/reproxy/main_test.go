package main

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// TestVersionRequested pins the --version guard: exactly a leading
// `--version` triggers the banner; anything else falls through to flag
// parsing (where an unknown flag errors) or normal startup.
func TestVersionRequested(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"leading --version", []string{"--version"}, true},
		{"--version before other args", []string{"--version", "--listen", ":9090"}, true},
		{"no args", nil, false},
		{"empty slice", []string{}, false},
		{"other flag first", []string{"--listen", ":8080", "--version"}, false},
		{"unknown flag", []string{"--wat"}, false},
		{"single-dash -version is a different flag", []string{"-version"}, false},
		{"positional arg", []string{"serve"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionRequested(tc.args); got != tc.want {
				t.Errorf("versionRequested(%q) = %t, want %t", tc.args, got, tc.want)
			}
		})
	}
}

// TestVersionDefault pins the default stamp: a binary built without the
// -ldflags "-X main.version=..." injection reports "dev".
func TestVersionDefault(t *testing.T) {
	if version != "dev" {
		t.Errorf("unstamped version = %q, want \"dev\"", version)
	}
}

// TestPrintVersionOutput pins the banner shape the CI smoke test asserts
// against (`docker run <img> --version` output == the expected version).
func TestPrintVersionOutput(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v1.2.3"
	var buf bytes.Buffer
	printVersion(&buf)
	if got, want := buf.String(), "v1.2.3\n"; got != want {
		t.Errorf("printVersion output = %q, want %q", got, want)
	}

	version = "dev"
	buf.Reset()
	printVersion(&buf)
	if got, want := buf.String(), "dev\n"; got != want {
		t.Errorf("printVersion output = %q, want %q", got, want)
	}
}

// TestRunVersionGoesToStdout pins the stream: the banner must land on the
// writer main passes (stdout), because CI captures stdout — a version
// printed to stderr would make `--version` output empty in `$(...)`
// capture.
func TestRunVersionGoesToStdout(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	version = "v0.0.0-smoke"

	var out bytes.Buffer
	if !runVersion(&out) {
		t.Error("runVersion reported failure")
	}
	if got, want := out.String(), "v0.0.0-smoke\n"; got != want {
		t.Errorf("runVersion wrote %q, want %q", got, want)
	}
}

// TestMainVersionBannerOnStdout exercises main itself, end to end: the
// test binary re-executes itself as a child (the GO_WANT_VERSION_PROCESS
// handshake is the standard reentrancy guard) with --version and asserts
// the banner arrives on STDOUT — the exact stream CI's
// `docker run <img> --version` captures. A mutation pointing main's
// call site at os.Stderr is caught here (mutation-scan verified).
func TestMainVersionBannerOnStdout(t *testing.T) {
	if os.Getenv("GO_WANT_VERSION_PROCESS") == "1" {
		// Child: present main with the argv a real `reproxy --version`
		// invocation would have (the test binary's -test.* flags are
		// not main's concern).
		os.Args = []string{"reproxy", "--version"}
		main()
		return // unreachable in practice: main exits
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestMainVersionBannerOnStdout$")
	cmd.Env = append(os.Environ(), "GO_WANT_VERSION_PROCESS=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	// main must exit 0 after the banner: a --version that "fails" would
	// break script usage (`reproxy --version` in an if-condition).
	if runErr != nil {
		t.Errorf("child did not exit cleanly: %v (stderr: %q)", runErr, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Errorf("version banner did not reach stdout (stderr was %q)", stderr.String())
	}
}
