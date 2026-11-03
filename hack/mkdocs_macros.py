# Variables for the documentation site (mkdocs.yml's macros plugin), so
# that no page carries a version to keep up to date:
#
#   {{{ gcr_version }}}   the release the site documents, v0.1.8
#   {{{ gcr_image }}}     the installer image of that release
#
# GCR_VERSION sets the version; .github/workflows/docs.yml passes the
# release's tag when it publishes the site for a release, and the latest
# release's tag when it publishes from main. Without it (a local `mkdocs
# serve`), the latest release tag in the repository is used.
import os
import subprocess

IMAGE = "ghcr.io/makhov/gcr"


def define_env(env):
    version = release_version()
    env.variables.gcr_version = version
    env.variables.gcr_image = f"{IMAGE}:{version}"


def release_version():
    version = os.environ.get("GCR_VERSION", "").strip()
    if version:
        return version
    try:
        tags = subprocess.run(
            ["git", "tag", "--list", "v[0-9]*", "--sort=-version:refname"],
            capture_output=True,
            check=True,
            text=True,
        ).stdout.split()
    except (OSError, subprocess.CalledProcessError) as err:
        raise RuntimeError(
            f"cannot read the release tags ({err}): set GCR_VERSION"
        ) from err
    if not tags:
        raise RuntimeError("no release tag (vX.Y.Z) here: set GCR_VERSION")
    return tags[0]
