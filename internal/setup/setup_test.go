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
				changed, err := registerContainerd(config, tc.major, quiet)
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 && changed {
					t.Fatal("a second run changed something")
				}
			}
			dropIn := read(t, filepath.Join(confd, "gcr.toml"))
			wantTable := "io.containerd.cri.v1.runtime"
			if tc.major == 1 {
				wantTable = "io.containerd.grpc.v1.cri"
			}
			if !strings.Contains(dropIn, wantTable) || !strings.Contains(dropIn, RuntimeType) {
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
