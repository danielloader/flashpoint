package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noEnv(string) string { return "" }

func ptr(n int) *int { return &n }

func TestParse(t *testing.T) {
	f, err := Parse([]byte(`
[api]
main = "./cmd/server"
port = 8511
build_flags = ["-tags=dev"]
args = ["-v"]
env = { LOG_LEVEL = "debug" }
stop_timeout = "3s"

[web]
dir = "frontend"
port = 5511

[watch]
include = ["config/**/*.yaml"]
debounce = "50ms"
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.API.Main != "./cmd/server" || *f.API.Port != 8511 || f.API.StopTimeout.Duration != 3*time.Second {
		t.Fatalf("api: %+v", f.API)
	}
	if f.Web.Dir != "frontend" || *f.Web.Port != 5511 || f.Watch.Debounce.Duration != 50*time.Millisecond {
		t.Fatalf("web/watch: %+v %+v", f.Web, f.Watch)
	}
	if f.API.Env["LOG_LEVEL"] != "debug" {
		t.Fatalf("env %v", f.API.Env)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("[api]\nmian = \"./cmd/x\"\n"))
	if err == nil || !strings.Contains(err.Error(), "mian") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRejectsBadDuration(t *testing.T) {
	if _, err := Parse([]byte("[watch]\ndebounce = \"soon\"\n")); err == nil {
		t.Fatal("want an error")
	}
}

func project(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/p\n\ngo 1.26\n")
	write(t, filepath.Join(root, "cmd", "api", "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(root, "internal", "x", "x.go"), "package x\n")
	write(t, filepath.Join(root, "web", "vite.config.ts"), "export default {}\n")
	write(t, filepath.Join(root, "web", "package.json"), `{"scripts":{"dev":"vite"}}`)
	write(t, filepath.Join(root, "web", "pnpm-lock.yaml"), "")
	return root
}

func TestResolveDetects(t *testing.T) {
	root := project(t)
	p, err := Resolve(root, File{}, Overrides{}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if p.Main != "./cmd/api" {
		t.Errorf("main = %q", p.Main)
	}
	if p.APIPort != DefaultAPIPort || p.StopSignal != syscall.SIGINT || p.Health != "/" {
		t.Errorf("api defaults: %d %v %q", p.APIPort, p.StopSignal, p.Health)
	}
	if p.Web == nil {
		t.Fatal("no web app found")
	}
	want := []string{"pnpm", "run", "dev", "--port", "5173", "--strictPort"}
	if !slices.Equal(p.Web.Argv, want) || !p.Web.PortOnArgv {
		t.Errorf("argv = %q", p.Web.Argv)
	}
}

func TestResolvePrecedence(t *testing.T) {
	root := project(t)
	f := File{API: API{Port: ptr(8512)}, Web: Web{Port: ptr(5512)}}
	env := func(k string) string {
		return map[string]string{"FLASHPOINT_API_PORT": "8513"}[k]
	}
	p, err := Resolve(root, f, Overrides{WebPort: ptr(5514)}, env)
	if err != nil {
		t.Fatal(err)
	}
	if p.APIPort != 8513 {
		t.Errorf("env should beat the file: api port %d", p.APIPort)
	}
	if p.Web.Port != 5514 {
		t.Errorf("flag should beat the file: web port %d", p.Web.Port)
	}
}

func TestResolveCommandPlaceholders(t *testing.T) {
	root := project(t)
	f := File{API: API{Port: ptr(8515)}, Web: Web{Command: "bun x vite --port {port}", Port: ptr(5515)}}
	p, err := Resolve(root, f, Overrides{}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Web.Argv[2]; got != "bun x vite --port 5515" {
		t.Errorf("command = %q", got)
	}
}

func TestResolveSamePortIsAnError(t *testing.T) {
	root := project(t)
	if _, err := Resolve(root, File{}, Overrides{APIPort: ptr(5516), WebPort: ptr(5516)}, noEnv); err == nil {
		t.Fatal("want an error")
	}
}

func TestResolveWatch(t *testing.T) {
	root := project(t)
	p, _ := Resolve(root, File{}, Overrides{}, noEnv)
	if !p.Watch {
		t.Fatal("watching should be on by default")
	}
	off := false
	if p, _ := Resolve(root, File{Watch: Watch{Enabled: &off}}, Overrides{}, noEnv); p.Watch {
		t.Fatal("watch.enabled = false should turn it off")
	}
	if p, _ := Resolve(root, File{}, Overrides{NoWatch: true}, noEnv); p.Watch {
		t.Fatal("--watch=false should turn it off")
	}
}

func TestResolveNoWeb(t *testing.T) {
	root := project(t)
	p, err := Resolve(root, File{}, Overrides{NoWeb: true}, noEnv)
	if err != nil || p.Web != nil {
		t.Fatalf("web = %+v, err %v", p.Web, err)
	}
}

func TestDetectMainAmbiguous(t *testing.T) {
	root := project(t)
	write(t, filepath.Join(root, "cmd", "worker", "main.go"), "package main\n\nfunc main() {}\n")
	_, err := DetectMain(root)
	if err == nil || !strings.Contains(err.Error(), "./cmd/api, ./cmd/worker") {
		t.Fatalf("err = %v", err)
	}
}

func TestDetectMainPrefersRoot(t *testing.T) {
	root := project(t)
	write(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	if m, err := DetectMain(root); err != nil || m != "." {
		t.Fatalf("main = %q, %v", m, err)
	}
}

func TestPackageManager(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"web/package.json": `{}`}, "npm"},
		{map[string]string{"web/package.json": `{}`, "web/yarn.lock": ""}, "yarn"},
		{map[string]string{"web/package.json": `{}`, "web/bun.lock": ""}, "bun"},
		{map[string]string{"web/package.json": `{}`, "pnpm-lock.yaml": ""}, "pnpm"},
		{map[string]string{"web/package.json": `{"packageManager":"yarn@4.1.0"}`, "web/package-lock.json": ""}, "yarn"},
	}
	for _, c := range cases {
		root := t.TempDir()
		for name, body := range c.files {
			write(t, filepath.Join(root, name), body)
		}
		if got := PackageManager(filepath.Join(root, "web"), root); got != c.want {
			t.Errorf("%v: got %s, want %s", c.files, got, c.want)
		}
	}
}

func TestWebCommandOnlyAddsPortToVite(t *testing.T) {
	cases := map[string]bool{
		"vite":                     true,
		"vite --host":              true,
		"vite dev":                 true,
		"vite build":               false,
		"node server.js":           false,
		"vite & tsc --watch":       false,
		"concurrently vite tsc -w": false,
	}
	for script, want := range cases {
		if got := isVite(script); got != want {
			t.Errorf("isVite(%q) = %v", script, got)
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "package.json"), `{"scripts":{"dev":"vite"}}`)
	argv, ok, err := WebCommand(dir, "npm", 5517)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if want := []string{"npm", "run", "dev", "--", "--port", "5517", "--strictPort"}; !slices.Equal(argv, want) {
		t.Errorf("npm argv = %q", argv)
	}
}

func TestFindRoot(t *testing.T) {
	root := project(t)
	got, err := FindRoot(filepath.Join(root, "internal", "x"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(root)
	if g, _ := filepath.EvalSymlinks(got); g != want {
		t.Fatalf("root = %s, want %s", got, root)
	}
}

func TestParseSignal(t *testing.T) {
	for _, s := range []string{"SIGTERM", "term", "TERM"} {
		if sig, err := ParseSignal(s); err != nil || sig != syscall.SIGTERM {
			t.Errorf("%s: %v %v", s, sig, err)
		}
	}
	if _, err := ParseSignal("SIGWINCH"); err == nil {
		t.Error("want an error")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"10MB": 10 << 20, "512kb": 512 << 10, "1GB": 1 << 30, "300": 300, "7 B": 7} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseSize("lots"); err == nil {
		t.Error("want an error")
	}
}

func TestResolveLogs(t *testing.T) {
	root := project(t)
	off := false
	f := File{Logs: Logs{Dir: ".flashpoint/logs", Web: "web.txt", Timestamps: &off, MaxSize: Size{1 << 20}}}
	p, err := Resolve(root, f, Overrides{Logs: Logs{API: "/tmp/api.log"}}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"api":        "/tmp/api.log",
		"web":        filepath.Join(root, "web.txt"),
		"flashpoint": filepath.Join(root, ".flashpoint", "logs", "flashpoint.log"),
		"all":        filepath.Join(root, ".flashpoint", "logs", "all.log"),
	}
	for k, v := range want {
		if p.Logs.Paths[k] != v {
			t.Errorf("%s = %q, want %q", k, p.Logs.Paths[k], v)
		}
	}
	if p.Logs.Timestamps || p.Logs.MaxSize != 1<<20 {
		t.Errorf("logs %+v", p.Logs)
	}
	if p, _ := Resolve(root, File{}, Overrides{}, noEnv); len(p.Logs.Paths) != 0 || !p.Logs.Timestamps {
		t.Errorf("default logs %+v", p.Logs)
	}
}
