#!/bin/bash
# Installs a GitHub Actions self-hosted runner for the tests that need
# what a GitHub-hosted runner cannot give them: the BPF LSM, which has to
# be in the kernel's lsm= list at boot (internal/netpolicy). Run as root
# on an Ubuntu host you are willing to let the repository's workflows
# execute code on; makhov/gcr is private, so that is everyone with write
# access to it.
#
#   REPO=makhov/gcr TOKEN=$(gh api -X POST \
#     repos/makhov/gcr/actions/runners/registration-token -q .token) \
#     sudo -E ./hack/runner-setup.sh
#
# The token is short-lived (an hour). The runner registers with the labels
# the bpf-lsm workflow asks for, runs as a systemd service under RUNNER_USER,
# and comes back after a reboot. Re-running it is safe: it reconfigures the
# same runner.
set -euo pipefail

REPO=${REPO:?set REPO, e.g. makhov/gcr}
TOKEN=${TOKEN:?set TOKEN to a runner registration token}
RUNNER_USER=${RUNNER_USER:-$(id -un 1000 2>/dev/null || echo ubuntu)}
RUNNER_NAME=${RUNNER_NAME:-$(hostname)}
RUNNER_DIR=${RUNNER_DIR:-/opt/actions-runner}
RUNNER_VERSION=${RUNNER_VERSION:-2.338.2}
LABELS=${LABELS:-self-hosted,linux,gcr-bpf}
LSM=${LSM:-lockdown,capability,landlock,yama,apparmor,bpf,ima,evm}

case "$(dpkg --print-architecture)" in
arm64) arch=arm64 ;;
amd64) arch=x64 ;;
*) echo "unsupported architecture: $(dpkg --print-architecture)" >&2; exit 1 ;;
esac

echo "== dependencies"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# The runner needs curl, tar and git; the tests need the rest, which
# hack/ci-setup.sh installs as well when it runs in the job.
apt-get install -y -qq curl tar git jq sudo

echo "== the BPF LSM in the kernel's lsm= list"
if grep -qw bpf /sys/kernel/security/lsm; then
	echo "already active: $(cat /sys/kernel/security/lsm)"
else
	printf 'GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT lsm=%s"\n' "$LSM" \
		>/etc/default/grub.d/90-gcr-bpf-lsm.cfg
	update-grub
	echo "NOTE: reboot this host before the first job; jobs would otherwise fail"
	echo "      with GCR_E2E_REQUIRE_BPF set, which is what the workflow wants."
fi

echo "== passwordless sudo for $RUNNER_USER (the tests run containers as root)"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$RUNNER_USER" >/etc/sudoers.d/90-gcr-runner
chmod 0440 /etc/sudoers.d/90-gcr-runner

echo "== runner $RUNNER_VERSION in $RUNNER_DIR"
mkdir -p "$RUNNER_DIR"
if [ ! -x "$RUNNER_DIR/config.sh" ]; then
	curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${arch}-${RUNNER_VERSION}.tar.gz" |
		tar -xz -C "$RUNNER_DIR"
fi
chown -R "$RUNNER_USER:$RUNNER_USER" "$RUNNER_DIR"
"$RUNNER_DIR/bin/installdependencies.sh" >/dev/null

if [ -f "$RUNNER_DIR/.runner" ]; then
	echo "== already registered; reconfiguring"
	systemctl stop "actions.runner.$(echo "$REPO" | tr / -).$RUNNER_NAME.service" 2>/dev/null || true
	sudo -u "$RUNNER_USER" "$RUNNER_DIR/config.sh" remove --token "$TOKEN" >/dev/null 2>&1 || true
fi
sudo -u "$RUNNER_USER" "$RUNNER_DIR/config.sh" \
	--unattended --replace \
	--url "https://github.com/$REPO" --token "$TOKEN" \
	--name "$RUNNER_NAME" --labels "$LABELS" --work _work

echo "== systemd service"
cd "$RUNNER_DIR" # svc.sh refuses to run from anywhere else
./svc.sh install "$RUNNER_USER"
./svc.sh start
./svc.sh status | head -5

echo
echo "Registered $RUNNER_NAME with labels $LABELS."
grep -qw bpf /sys/kernel/security/lsm ||
	echo "Reboot before running the bpf-lsm workflow."
