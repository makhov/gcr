// Package imagetar extracts tar streams of CRIU images that come from
// elsewhere (a migration's source, a registry) as root, without letting
// them reach outside their directory.
//
// CRIU images are regular files, and one link: "parent", to the images a
// dump was incremental on, a round directory beside them ("../2"). An
// archive is extracted into a directory with nothing but regular files,
// directories and that link; every file is created anew (O_EXCL) and no
// path is resolved through a link (os.Root), so it cannot overwrite what
// is there or write through a link it, or an earlier archive, planted.
package imagetar

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"

	"github.com/containerd/errdefs"
)

// parentTarget is what a parent link may point at.
var parentTarget = regexp.MustCompile(`^\.\./[0-9]+$`)

// Extract extracts the tar stream r into dir, creating dir if needed.
func Extract(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if name == "." {
			continue
		}
		if !localName(name) {
			return refuse(h.Name, "not a name inside the directory")
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
		case tar.TypeSymlink:
			if name != "parent" || !parentTarget.MatchString(h.Linkname) {
				return refuse(h.Name, "a link other than CRIU's parent (-> %q)", h.Linkname)
			}
			// The target's own dump may have made it already.
			if got, err := root.Readlink(name); err == nil {
				if got != h.Linkname {
					return refuse(h.Name, "parent is %q here, %q in the archive", got, h.Linkname)
				}
				continue
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		default:
			return refuse(h.Name, "not a regular file, directory or CRIU's parent link (type %q)", h.Typeflag)
		}
	}
}

// localName says whether name stays inside the directory as it is:
// relative, and with no ".." anywhere.
func localName(name string) bool {
	if path.IsAbs(name) {
		return false
	}
	for _, part := range splitPath(name) {
		if part == ".." {
			return false
		}
	}
	return true
}

func splitPath(name string) []string {
	var parts []string
	for name != "" {
		dir, file := path.Split(name)
		parts = append(parts, file)
		name = path.Clean(dir)
		if name == "." || name == "/" {
			break
		}
	}
	return parts
}

func refuse(name, why string, args ...any) error {
	return fmt.Errorf("image archive: %s: %s: %w", name, fmt.Sprintf(why, args...), errdefs.ErrPermissionDenied)
}
