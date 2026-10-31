package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// distro is a Kubernetes distribution that runs a containerd of its own,
// with a configuration it generates on every start and a directory of
// drop-ins that configuration imports.
type distro struct {
	Name string
	// Dir holds the generated config.toml and the drop-in directories.
	Dir string
	// Units are the systemd units that run it (server or agent), one of
	// which is active on a node.
	Units []string
}

// Their containerd configurations, k3s's and RKE2's alike: since
// v1.36.0+k3s1 (and the 2026-03 backports to v1.33 and v1.34), the base
// template imports config-v3.toml.d (config.toml.d for a version 2
// configuration) beside it.
var distros = []distro{
	{Name: "k3s", Dir: "/var/lib/rancher/k3s/agent/etc/containerd", Units: []string{"k3s", "k3s-agent"}},
	{Name: "RKE2", Dir: "/var/lib/rancher/rke2/agent/etc/containerd", Units: []string{"rke2-server", "rke2-agent"}},
}

// detectDistro is the distribution whose containerd runs here, if any.
func detectDistro() *distro {
	for i := range distros {
		if _, err := os.Stat(filepath.Join(distros[i].Dir, "config.toml")); err == nil {
			return &distros[i]
		}
	}
	return nil
}

// dropInDir is where d's containerd imports drop-ins from, as the
// configuration it generated says, and that configuration's version.
func (d *distro) dropInDir() (string, int, error) {
	config := filepath.Join(d.Dir, "config.toml")
	b, err := os.ReadFile(config)
	if err != nil {
		return "", 0, err
	}
	version := rootVersion(b, 2)
	dir := filepath.Join(d.Dir, "config.toml.d")
	if version >= 3 {
		dir = filepath.Join(d.Dir, "config-v3.toml.d")
	}
	if m := importsLine.FindSubmatch(b); m == nil || !strings.Contains(string(m[1]), dir) {
		return "", 0, fmt.Errorf("%s does not import %s: %s releases from v1.36.0 (v1.33 and v1.34 since 2026-03) do, "+
			"unless a custom config-v3.toml.tmpl replaces the base template. Upgrade, or add this to that template:\n%s",
			config, dir, d.Name, containerdDropIn(version, "/usr/local/bin/containerd-shim-gcr-v2"))
	}
	return dir, version, nil
}

// unit is the systemd unit running d on this node.
func (d *distro) unit() (string, error) {
	for _, u := range d.Units {
		if exec.Command("systemctl", "is-active", "--quiet", u).Run() == nil {
			return u, nil
		}
	}
	return "", fmt.Errorf("none of %s is active", strings.Join(d.Units, ", "))
}

// registerDistro writes gcr's drop-in where d's containerd imports it and
// restarts d, which restarts its containerd (running containers carry
// on: its units kill only the main process). If d does not come back
// with it, the drop-in goes and d restarts again.
func registerDistro(d *distro, shim string, noRestart bool, say func(string, ...any)) error {
	dir, version, err := d.dropInDir()
	if err != nil {
		return err
	}
	dropIn := filepath.Join(dir, "gcr.toml")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	changed, err := writeIfChanged(dropIn, []byte(containerdDropIn(version, shim)), 0o644)
	if err != nil || !changed {
		return err
	}
	say("wrote %s", dropIn)
	if noRestart {
		say("%s applies it on its next start", d.Name)
		return nil
	}
	unit, err := d.unit()
	if err != nil {
		return fmt.Errorf("%w: restart %s yourself to apply %s", err, d.Name, dropIn)
	}
	if err := restartActive(unit); err != nil {
		_ = os.Remove(dropIn)
		if rerr := restartActive(unit); rerr != nil {
			return fmt.Errorf("%w; removed %s, and restarting %s without it failed too: %v", err, dropIn, unit, rerr)
		}
		return fmt.Errorf("%w; removed %s and restarted %s without it", err, dropIn, unit)
	}
	say("restarted %s (running containers carry on)", unit)
	return nil
}

// restartActive restarts unit and waits for it to be active again, as
// k3s and RKE2 are once their containerd and kubelet run.
func restartActive(unit string) error {
	if err := restart(unit); err != nil {
		return err
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil {
			return nil
		}
	}
	return fmt.Errorf("%s is not active 2 minutes after a restart", unit)
}
