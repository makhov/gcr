//go:build linux

// gcr is the command line for gcr runtime features outside containerd's API.
//
//	gcr [-n namespace] timeline ID
//	gcr [-n namespace] rewind ID POINT
//	gcr [-n namespace] snapshot ID DIR
//	gcr snapshot-users DIR
//	gcr snapshot-rm DIR...
//	gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF
//	gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR
//	gcr [-n namespace] migrate [-token TOKEN] [-post-copy] ID TARGET-URL
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/snapshot/registry"
)

func main() {
	ns := flag.String("n", "default", "containerd namespace")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gcr [-n namespace] timeline ID\n"+
			"       gcr [-n namespace] rewind ID POINT\n"+
			"       gcr [-n namespace] snapshot ID DIR\n"+
			"       gcr snapshot-users DIR\n"+
			"       gcr snapshot-rm DIR...\n"+
			"       gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF\n"+
			"       gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR\n"+
			"       gcr [-n namespace] migrate [-token TOKEN] [-post-copy] ID TARGET-URL\n")
	}
	flag.Parse()
	args := flag.Args()
	if err := run(context.Background(), *ns, args); err != nil {
		fmt.Fprintln(os.Stderr, "gcr:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, ns string, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "timeline":
		points, err := control.NewClient(ns, args[1]).Timeline(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "POINT\tTIME\tBEFORE")
		for i := len(points) - 1; i >= 0; i-- {
			p := points[i]
			call := fmt.Sprintf("%s(%q", p.Syscall, p.Path)
			if len(p.Argv) > 0 {
				call += ", [" + quoteAll(p.Argv) + "]"
			}
			fmt.Fprintf(w, "#%d\t%s\t%s)\n", p.ID, p.Time.Format("15:04:05.000"), call)
		}
		return w.Flush()
	case len(args) == 3 && args[0] == "rewind":
		point, err := strconv.Atoi(strings.TrimPrefix(args[2], "#"))
		if err != nil {
			return fmt.Errorf("bad point %q", args[2])
		}
		return control.NewClient(ns, args[1]).Rewind(ctx, point)
	case len(args) == 3 && args[0] == "snapshot":
		// The container keeps running; containers created with the
		// annotation gcr.dev/fork-from=DIR start from the snapshot.
		return control.NewClient(ns, args[1]).Snapshot(ctx, args[2])
	case len(args) == 2 && args[0] == "snapshot-users":
		// The containers that mount it and the snapshots taken of them.
		users, err := snapshot.Users(args[1])
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KIND\tUSER\tSINCE")
		for _, u := range users {
			name := u.Path
			if u.Kind == snapshot.Container {
				name = u.Namespace + "/" + u.ID
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", u.Kind, name, u.Since.Local().Format(time.DateTime))
		}
		return w.Flush()
	case len(args) >= 2 && args[0] == "snapshot-rm":
		// Refused while anything uses it.
		var errs []error
		for _, dir := range args[1:] {
			errs = append(errs, snapshot.Remove(dir))
		}
		return errors.Join(errs...)
	case len(args) >= 1 && (args[0] == "snapshot-push" || args[0] == "snapshot-pull"):
		// Through an OCI registry: REF is like registry.example.com/snapshots/warm:v1.
		fs := flag.NewFlagSet(args[0], flag.ExitOnError)
		plainHTTP := fs.Bool("plain-http", false, "speak HTTP, not HTTPS, to the registry (always for loopback)")
		store := fs.String("store", registry.DefaultStore, "where the layers beneath pulled snapshots go")
		lazy := fs.Bool("lazy", false, "snapshot-pull: mount a nydus snapshot's parts, reading from the registry what is read (needs nydusd)")
		format := fs.String("format", registry.FormatTar, "snapshot-push: tar, or nydus (nodes can then read parts lazily; needs nydus-image and nydusd)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		ref := fs.Arg(1)
		if args[0] == "snapshot-pull" {
			ref = fs.Arg(0)
		}
		repo, err := registry.Repository(ref, *plainHTTP)
		if err != nil {
			return err
		}
		tag := repo.Reference.Reference
		if tag == "" {
			tag = "latest"
		}
		if args[0] == "snapshot-pull" {
			pulled, err := registry.Pull(ctx, repo, tag, fs.Arg(1), *store, registry.PullOptions{Lazy: *lazy})
			if err != nil {
				return err
			}
			if pulled.Lazy {
				fmt.Println("mounted lazily: nydusd reads it from the registry until gcr snapshot-rm; after a reboot, pull it again")
			}
			if pulled.From != 0 {
				fmt.Printf("moved from user namespace range %d to %d (that one is in use here)\n", pulled.From, pulled.Range.Base)
			}
			return nil
		}
		desc, err := registry.Push(ctx, fs.Arg(0), repo, tag, *format)
		if err != nil {
			return err
		}
		fmt.Printf("%s@%s\n", repo.Reference.Registry+"/"+repo.Reference.Repository, desc.Digest)
		return nil
	case len(args) >= 1 && args[0] == "migrate":
		fs := flag.NewFlagSet("migrate", flag.ExitOnError)
		token := fs.String("token", os.Getenv("GCR_MIGRATE_TOKEN"), "token the target expects (default $GCR_MIGRATE_TOKEN)")
		postCopy := fs.Bool("post-copy", false, "restore the target before its memory arrives; it fetches the memory from the source as it runs")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		res, err := control.NewClient(ns, fs.Arg(0)).Migrate(ctx, fs.Arg(1), *token, migrate.Options{PostCopy: *postCopy})
		if err != nil {
			return err
		}
		if res.PostCopy {
			fmt.Printf("post-copy: downtime %s, memory fetched by %s\n", res.Downtime.Round(time.Millisecond), res.PostCopyTook.Round(time.Millisecond))
			return nil
		}
		if res.PreCopyFailed != "" {
			fmt.Printf("pre-copy failed, went on stop-and-copy: %s\n", res.PreCopyFailed)
		}
		if res.MemoryTracking {
			fmt.Printf("pre-copy rounds: %d (pages %v)\n", len(res.PreCopyPages), res.PreCopyPages)
		} else {
			fmt.Println("stop-and-copy (kernel cannot track dirty memory)")
		}
		fmt.Printf("downtime: %s\n", res.Downtime.Round(time.Millisecond))
		return nil
	default:
		flag.Usage()
		os.Exit(2)
		return nil
	}
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}
