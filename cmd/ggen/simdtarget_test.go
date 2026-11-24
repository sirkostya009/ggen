package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCLIWithEnv runs the binary with env appended to the test's environment.
func runCLIWithEnv(t *testing.T, bin, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// TestSIMDTarget pins that the default tier and the explicit tiers follow the
// target the go command resolves: GOARCH, the go env file and `no` prefixes
// in GOEXPERIMENT.
func TestSIMDTarget(t *testing.T) {
	t.Parallel()
	bin := buildCLI(t)

	goenv := filepath.Join(t.TempDir(), "goenv")
	if err := os.WriteFile(goenv, []byte("GOEXPERIMENT=simd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		env     []string
		args    []string
		wantAVX bool
		wantErr string
	}{
		{name: "Amd64Experiment_DefaultAVX", env: []string{"GOARCH=amd64", "GOEXPERIMENT=simd"}, wantAVX: true},
		{name: "Arm64Experiment_DefaultOff", env: []string{"GOARCH=arm64", "GOEXPERIMENT=simd"}},
		{
			name:    "Arm64Experiment_ExplicitTierRejected",
			env:     []string{"GOARCH=arm64", "GOEXPERIMENT=simd"},
			args:    []string{"-simd=avx2"},
			wantErr: "requires GOARCH=amd64",
		},
		{name: "NegatedExperiment_DefaultOff", env: []string{"GOARCH=amd64", "GOEXPERIMENT=simd,nosimd"}},
		{
			name:    "NegatedExperiment_ExplicitTierRejected",
			env:     []string{"GOARCH=amd64", "GOEXPERIMENT=simd,nosimd"},
			args:    []string{"-simd=avx2"},
			wantErr: "requires GOEXPERIMENT=simd",
		},
		{name: "GoEnvFile_DefaultAVX", env: []string{"GOARCH=amd64", "GOEXPERIMENT=", "GOENV=" + goenv}, wantAVX: true},
		{name: "GoEnvFile_ExplicitTierAccepted", env: []string{"GOARCH=amd64", "GOEXPERIMENT=", "GOENV=" + goenv}, args: []string{"-simd=avx2"}, wantAVX: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFixture(t, filepath.Join(dir, "msg.go"), minimalStruct)
			out, err := runCLIWithEnv(t, bin, dir, tc.env, append(tc.args, "msg.go")...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(out, tc.wantErr) {
					t.Fatalf("want error containing %q, got err=%v:\n%s", tc.wantErr, err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("ggen: %v\n%s", err, out)
			}
			body := mustReadOutput(t, filepath.Join(dir, "msg_ggen.go"))
			if got := strings.Contains(body, "AVX"); got != tc.wantAVX {
				t.Fatalf("AVX tier in output = %v, want %v:\n%s", got, tc.wantAVX, body)
			}
		})
	}
}
