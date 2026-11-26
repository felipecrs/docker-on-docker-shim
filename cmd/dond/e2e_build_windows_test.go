package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testImage = "dond-test-build"

func TestBuild(t *testing.T) {
	docker := buildShim(t)
	t.Cleanup(func() { _ = exec.Command(docker, "rmi", "-f", testImage).Run() })

	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(`context dir\hello.txt`, "from context")
	write(`lib\lib.txt`, "from lib")
	secret := write("secret.txt", "from secret")
	context, lib := filepath.Join(dir, "context dir"), filepath.Join(dir, "lib")
	dockerfile := write(`docker\Dockerfile`, strings.Join([]string{
		"FROM " + image + " AS base",
		"ARG DIR",
		"COPY hello.txt /out/hello.txt",
		"COPY --from=lib lib.txt /out/lib.txt",
		`RUN --mount=type=secret,id=file --mount=type=secret,id=var sh -c 'cat /run/secrets/file > /out/file.txt; cat /run/secrets/var > /out/var.txt; printf %s "$DIR" > /out/dir.txt'`,
		"FROM scratch",
		"COPY --from=base /out /",
	}, "\n"))
	options := []string{
		"-f", dockerfile, "--build-context", "lib=" + lib, "--secret", "id=file,src=" + secret,
		"--secret", "id=var,env=SHIM_SECRET", "--build-arg", `DIR=C:\kept`,
	}
	expected := map[string]string{
		"hello.txt": "from context", "lib.txt": "from lib", "file.txt": "from secret", "var.txt": "a b $c", "dir.txt": `C:\kept`,
	}

	run := func(t *testing.T, stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(docker, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SHIM_SECRET=a b $c", "BUILDKIT_PROGRESS=plain")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	check := func(t *testing.T, out string) {
		t.Helper()
		for name, want := range expected {
			if got, err := os.ReadFile(filepath.Join(out, name)); err != nil || string(got) != want {
				t.Errorf("%s: got %q %v, want %q", name, got, err, want)
			}
		}
	}

	for _, command := range [][]string{{"build"}, {"buildx", "build"}, {"image", "build"}, {"builder", "build"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			args := append(append([]string{}, command...), options...)
			run(t, "", append(args, "--output", "type=local,dest="+out, context)...)
			check(t, out)
		})
	}
	t.Run("output as a path", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		run(t, "", append(append([]string{"buildx", "build"}, options...), "-o", out, context)...)
		check(t, out)
	})
	t.Run("relative paths", func(t *testing.T) {
		run(t, "", "buildx", "build", "-f", `.\docker\Dockerfile`, "--build-context", `lib=.\lib`, "--secret", `id=file,src=.\secret.txt`,
			"--secret", "id=var,env=SHIM_SECRET", "--build-arg", `DIR=C:\kept`, "-o", `.\relative`, `.\context dir`)
		check(t, filepath.Join(dir, "relative"))
	})
	t.Run("load, iidfile and metadata", func(t *testing.T) {
		iid, metadata := filepath.Join(dir, "iid.txt"), filepath.Join(dir, "metadata.json")
		run(t, "", append(append([]string{"buildx", "build"}, options...), "--load", "-t", testImage, "--iidfile", iid, "--metadata-file", metadata, context)...)
		if raw, err := os.ReadFile(iid); err != nil || !strings.HasPrefix(string(raw), "sha256:") {
			t.Errorf("iidfile: %q %v", raw, err)
		}
		if raw, err := os.ReadFile(metadata); err != nil || !strings.Contains(string(raw), "digest") {
			t.Errorf("metadata: %q %v", raw, err)
		}
		run(t, "", "image", "inspect", testImage)
	})
	t.Run("Dockerfile from standard input", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		run(t, "FROM scratch\nCOPY hello.txt /\n", "buildx", "build", "-f", "-", "-o", out, context)
		if got, err := os.ReadFile(filepath.Join(out, "hello.txt")); err != nil || string(got) != "from context" {
			t.Errorf("got %q %v", got, err)
		}
	})
	t.Run("bake", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		file := write("docker-bake.hcl", `target "app" {
  args = { DIR = "overridden" }
  secret = ["id=var,env=SHIM_SECRET"]
}
`)
		run(t, "", "buildx", "bake", "-f", file, "--allow", "fs="+dir, "--allow", "fs="+filepath.Dir(out),
			"--set", "*.context="+context, "--set", "*.dockerfile="+dockerfile, "--set", "*.contexts.lib="+lib,
			"--set", "*.secrets=id=file,src="+secret, "--set", "*.secrets+=id=var,env=SHIM_SECRET",
			"--set", `app.args.DIR=C:\kept`, "--set", "*.output=type=local,dest="+out, "app")
		check(t, out)
	})
	t.Run("local cache", func(t *testing.T) {
		cache := filepath.Join(t.TempDir(), "cache")
		args := append(append([]string{"buildx", "build"}, options...), "-o", "type=cacheonly", context)
		run(t, "", append(args, "--cache-to", "type=local,mode=max,dest="+cache)...)
		if _, err := os.Stat(filepath.Join(cache, "index.json")); err != nil {
			t.Error(err)
		}
		if out := run(t, "", append(args, "--cache-from", "type=local,src="+cache)...); !strings.Contains(out, "importing cache manifest from local:") {
			t.Errorf("the cache was not read: %s", out)
		}
	})
	t.Run("ssh", func(t *testing.T) {
		_, private, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		key := write("id_test", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
		run(t, "FROM "+image+"\nRUN --mount=type=ssh test -S \"$SSH_AUTH_SOCK\"\n",
			"buildx", "build", "--no-cache", "-f", "-", "--ssh", "default="+key, "-o", "type=cacheonly", context)
	})
	t.Run("buildx create", func(t *testing.T) {
		// Each step is its own wrapper, so the builder must outlive the pipe that created it.
		t.Setenv("BUILDX_CONFIG", t.TempDir())
		node := "buildx_buildkit_" + testImage + "0"
		t.Cleanup(func() {
			_ = exec.Command(docker, "buildx", "rm", testImage).Run()
			_ = exec.Command(docker, "rm", "-f", node).Run()
			_ = exec.Command(docker, "volume", "rm", "-f", node+"_state").Run()
		})
		config := write("buildkitd.toml", "[worker.oci]\n  max-parallelism = 3\n")
		run(t, "", "buildx", "create", "--name", testImage, "--driver", "docker-container", "--buildkitd-config", config, "--use")
		// inspect exits 0 when it cannot reach the builder.
		out := run(t, "", "buildx", "inspect", "--bootstrap")
		for _, want := range []string{`(?m)^Name:\s+` + testImage + `$`, `(?m)^Endpoint:\s+default$`, `(?m)^Status:\s+running$`, `max-parallelism = 3`} {
			if !regexp.MustCompile(want).MatchString(out) {
				t.Errorf("inspect has no %q: %s", want, out)
			}
		}
		dest := filepath.Join(t.TempDir(), "out")
		run(t, "", append(append([]string{"buildx", "build", "--builder", testImage}, options...), "-o", "type=local,dest="+dest, context)...)
		check(t, dest)
		run(t, "", "buildx", "rm", testImage)
		if out := run(t, "", "ps", "-a", "-q", "--filter", "name="+node) + run(t, "", "volume", "ls", "-q", "--filter", "name="+node); out != "" {
			t.Errorf("left behind: %s", out)
		}
	})
	t.Run("compose build", func(t *testing.T) {
		file := write("build.compose.yml", "services:\n  app:\n    image: "+testImage+"\n    build:\n      context: ./context dir\n      dockerfile_inline: |\n        FROM scratch\n        COPY hello.txt /\n")
		run(t, "", "compose", "-p", testImage, "-f", file, "build")
	})
}
