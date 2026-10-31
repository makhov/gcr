//go:build linux

package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quiet(string, ...any) {}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRegisterContainerd(t *testing.T) {
	for name, tc := range map[string]struct {
		config string // "" for none
		major  int
		want   []string // in config.toml afterwards; nil: no config.toml
	}{
		"no config, containerd 2": {major: 2},
		"version 2 config, containerd 2 (kind)": {
			config: "version = 2\n[plugins.\"io.containerd.grpc.v1.cri\".containerd]\n  snapshotter = \"overlayfs\"\n",
			major:  2,
			want:   []string{"version = 2\nimports = ['CONFD/*.toml']\n[plugins"},
		},
		"no config, containerd 1": {major: 1, want: []string{"version = 2", "imports = ['CONFD/*.toml']"}},
		"config without imports": {
			config: "version = 3\n[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]\n  runtime_type = 'io.containerd.runc.v2'\n",
			major:  2,
			want:   []string{"version = 3\nimports = ['CONFD/*.toml']\n[plugins", "runtimes.runc"},
		},
		"config with other imports": {
			config: "version = 3\nimports = ['/etc/other.toml']\n",
			major:  2,
			want:   []string{"imports = ['/etc/other.toml', 'CONFD/*.toml']"},
		},
		"config importing conf.d": {
			config: "version = 3\nimports = ['CONFD/*.toml']\n",
			major:  2,
			want:   []string{"version = 3\nimports = ['CONFD/*.toml']\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "config.toml")
			confd := filepath.Join(dir, "conf.d")
			if tc.config != "" {
				if err := os.WriteFile(config, []byte(strings.ReplaceAll(tc.config, "CONFD", confd)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 2 { // a second run changes nothing
				changed, _, err := registerContainerd(config, "/x/shim", tc.major, quiet)
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 && changed {
					t.Fatal("a second run changed something")
				}
			}
			dropIn := read(t, filepath.Join(confd, "gcr.toml"))
			// The drop-in's format is the configuration's: containerd
			// refuses one newer than what imports it.
			wantTable := "io.containerd.cri.v1.runtime"
			if tc.major == 1 || strings.Contains(tc.config, "version = 2") {
				wantTable = "io.containerd.grpc.v1.cri"
			}
			if !strings.Contains(dropIn, wantTable) || !strings.Contains(dropIn, RuntimeType) || !strings.Contains(dropIn, "runtime_path = '/x/shim'") {
				t.Fatalf("drop-in for containerd %d:\n%s", tc.major, dropIn)
			}
			if tc.want == nil {
				if _, err := os.Stat(config); err == nil {
					t.Fatal("wrote a config.toml where containerd's defaults import conf.d")
				}
				return
			}
			got := read(t, config)
			for _, w := range tc.want {
				if !strings.Contains(got, strings.ReplaceAll(w, "CONFD", confd)) {
					t.Fatalf("config.toml lacks %q:\n%s", w, got)
				}
			}
			if tc.config != "" && !strings.Contains(tc.config, "CONFD") {
				if b := read(t, config+".gcr-backup"); b != tc.config {
					t.Fatalf("backup:\n%s", b)
				}
			}
		})
	}
}

// Undo puts the configuration and conf.d back as they were, as when
// containerd would not load the new configuration.
func TestRegisterContainerdUndo(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.toml")
	old := "version = 3\n[plugins.'x']\n"
	if err := os.WriteFile(config, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, undo, err := registerContainerd(config, "/x/shim", 2, quiet)
	if err != nil || !changed {
		t.Fatalf("%v, %v", changed, err)
	}
	undo()
	if got := read(t, config); got != old {
		t.Fatalf("config.toml after undo:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "conf.d", "gcr.toml")); err == nil {
		t.Fatal("the drop-in is still there after undo")
	}
}

func TestRegisterK0s(t *testing.T) {
	dir := t.TempDir()
	k0sConfig = filepath.Join(dir, "containerd.toml")
	if err := os.WriteFile(k0sConfig, []byte("# k0s_managed=true\nversion = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := registerK0s("/x/shim", quiet); err != nil || !changed {
		t.Fatalf("%v, %v", changed, err)
	}
	if got := read(t, filepath.Join(dir, "containerd.d", "gcr.toml")); !strings.Contains(got, "version = 3") ||
		!strings.Contains(got, "io.containerd.cri.v1.runtime") {
		t.Fatalf("drop-in:\n%s", got)
	}
	if got := read(t, k0sConfig); got != "# k0s_managed=true\nversion = 3\n" {
		t.Fatalf("k0s's own configuration changed:\n%s", got)
	}
	// One taken over from k0s does not import containerd.d.
	if err := os.WriteFile(k0sConfig, []byte("version = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := registerK0s("/x/shim", quiet); err == nil || !strings.Contains(err.Error(), "not managed by k0s") {
		t.Fatalf("unmanaged: %v", err)
	}
}

func TestRegisterDocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte(`{"log-driver": "local", "runtimes": {"other": {"path": "/x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := registerDocker(path, quiet); err != nil || !changed {
		t.Fatalf("first run: %v, %v", changed, err)
	}
	if changed, err := registerDocker(path, quiet); err != nil || changed {
		t.Fatalf("second run: %v, %v", changed, err)
	}
	var cfg struct {
		LogDriver string                       `json:"log-driver"`
		Runtimes  map[string]map[string]string `json:"runtimes"`
	}
	if err := json.Unmarshal([]byte(read(t, path)), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.LogDriver != "local" || cfg.Runtimes["other"]["path"] != "/x" || cfg.Runtimes["gcr"]["runtimeType"] != RuntimeType {
		t.Fatalf("daemon.json: %+v", cfg)
	}
}

func TestKernelAtLeast(t *testing.T) {
	for release, want := range map[string]bool{
		"7.0.0-38-generic": true, "6.12.1": true, "6.11.9-arch1": false, "5.15.0": false, "x": false,
	} {
		if got := kernelAtLeast(release, 6, 12); got != want {
			t.Errorf("%s: %v", release, got)
		}
	}
}

func TestDistroDropInDir(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
		version            int
	}{
		{"v3 importing config-v3.toml.d", "version = 3\nimports = [\"DIR/config-v3.toml.d/*.toml\"]\n", "config-v3.toml.d", 3},
		{"v2 importing config.toml.d", "version = 2\nimports = [\"DIR/config.toml.d/*.toml\"]\n", "config.toml.d", 2},
		{"an older release, importing nothing", "version = 3\n", "", 0},
		{"a custom template importing elsewhere", "version = 3\nimports = [\"/etc/other/*.toml\"]\n", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := distro{Name: "k3s", Dir: t.TempDir()}
			config := strings.ReplaceAll(tc.config, "DIR", d.Dir)
			if err := os.WriteFile(filepath.Join(d.Dir, "config.toml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			dir, version, err := d.dropInDir()
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "does not import") {
					t.Fatalf("got %q, %v; want a refusal", dir, err)
				}
				return
			}
			if err != nil || dir != filepath.Join(d.Dir, tc.want) || version != tc.version {
				t.Fatalf("got %q version %d, %v; want %s version %d", dir, version, err, tc.want, tc.version)
			}
		})
	}
}
