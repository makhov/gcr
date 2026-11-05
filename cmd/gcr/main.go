//go:build linux

// gcr is the command line for gcr runtime features outside containerd's API.
//
//	gcr setup [-docker] [-no-restart] [-bin-dir DIR] [-containerd-config FILE] [-containerd-unit UNIT]
//	gcr doctor
//	gcr version
//	gcr [-n namespace] timeline ID
//	gcr [-n namespace] rewind ID POINT
//	gcr [-n namespace] point ID [LABEL]
//	gcr [-n namespace] snapshot [-stop] ID DIR
//	gcr [-n namespace] info ID
//	gcr snapshot-users DIR
//	gcr snapshot-rm DIR...
//	gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF
//	gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR
//	gcr [-n namespace] migrate [-token TOKEN] [-post-copy] ID TARGET-URL
//	gcr shutdown-guard [-plain-http] [-wait DURATION] REPOSITORY
//	gcr shutdown-plan -live|-checkpoint|-released POD-UID...
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/kube"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/nodeshutdown"
	"github.com/makhov/gcr/internal/payload"
	"github.com/makhov/gcr/internal/setup"
	"github.com/makhov/gcr/internal/snapimage"
	"github.com/makhov/gcr/internal/snapshot"
	"github.com/makhov/gcr/internal/snapshot/registry"
)

func main() {
	// gcr's CRIU, and the GNU tar setup installs beside it where the
	// node's is busybox's, which snapshot-push and -pull run too.
	setup.UseOwnCRIU()
	ns := flag.String("n", "default", "containerd namespace")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gcr setup [-docker] [-no-restart] [-bin-dir DIR] [-containerd-config FILE] [-containerd-unit UNIT]\n"+
			"       gcr doctor\n"+
			"       gcr version\n"+
			"       gcr [-n namespace] timeline ID\n"+
			"       gcr [-n namespace] rewind ID POINT\n"+
			"       gcr [-n namespace] point ID [LABEL]\n"+
			"       gcr [-n namespace] snapshot [-stop] ID DIR\n"+
			"       gcr [-n namespace] info ID\n"+
			"       gcr snapshot-users DIR\n"+
			"       gcr snapshot-rm DIR...\n"+
			"       gcr [-n namespace] snapshot-image [-plain-http] ID REF\n"+
			"       gcr snapshot-build [flags] IMAGE REF [-- COMMAND...]\n"+
			"       gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF\n"+
			"       gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR\n"+
			"       gcr [-n namespace] migrate [-token TOKEN] [-post-copy] ID TARGET-URL\n"+
			"       gcr shutdown-guard [-plain-http] [-wait DURATION] REPOSITORY\n"+
			"       gcr shutdown-plan -live|-checkpoint|-released POD-UID...\n")
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
	case len(args) >= 1 && args[0] == "setup":
		fs := flag.NewFlagSet("setup", flag.ExitOnError)
		o := setup.Options{Out: os.Stdout}
		fs.StringVar(&o.BinDir, "bin-dir", "/usr/local/bin", "where gcr and the shim go")
		fs.StringVar(&o.ContainerdConfig, "containerd-config", "/etc/containerd/config.toml", "containerd's configuration; gcr's goes in conf.d beside it")
		fs.StringVar(&o.ContainerdUnit, "containerd-unit", "containerd", "the systemd unit running containerd, restarted to apply the configuration")
		fs.BoolVar(&o.Docker, "docker", false, "register gcr with dockerd too, as runtime gcr")
		fs.BoolVar(&o.NoRestart, "no-restart", false, "do not restart containerd (or dockerd); the configuration applies on their next start")
		snapshotImages := fs.String("snapshot-images", "", "comma-separated images made of snapshots (gcr snapshot-image) the gcr handler restores: references, prefixes ending in *, or * for any")
		_ = fs.Parse(args[1:])
		for _, img := range strings.Split(*snapshotImages, ",") {
			if img = strings.TrimSpace(img); img != "" {
				o.SnapshotImages = append(o.SnapshotImages, img)
			}
		}
		if err := setup.Run(o); err != nil {
			return err
		}
		fmt.Println()
		return doctor()
	case len(args) == 1 && args[0] == "doctor":
		return doctor()
	case len(args) == 3 && args[0] == "kube-label":
		// For the installer DaemonSet (deploy/): label its node, from a pod
		// with a service account allowed to patch nodes.
		key, value, set := strings.Cut(args[2], "=")
		if !set {
			key = strings.TrimSuffix(key, "-")
		}
		return kube.LabelNode(ctx, args[1], key, value, set)
	case len(args) == 1 && args[0] == "version":
		fmt.Printf("gcr %s (%s)\n", version.Version, version.Revision)
		if v, err := payload.Read(payload.Versions); err == nil {
			// What it carries for gcr setup: the shim is built with it.
			for _, l := range strings.Split(strings.TrimSpace(string(v)), "\n")[1:] {
				fmt.Println("carries " + l)
			}
		}
		return nil
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
			call += ")"
			if p.Syscall == "" {
				call = p.Label // taken on request
			}
			fmt.Fprintf(w, "#%d\t%s\t%s\n", p.ID, p.Time.Format("15:04:05.000"), call)
		}
		return w.Flush()
	case len(args) == 3 && args[0] == "rewind":
		point, err := strconv.Atoi(strings.TrimPrefix(args[2], "#"))
		if err != nil {
			return fmt.Errorf("bad point %q", args[2])
		}
		if err := control.NewClient(ns, args[1]).Rewind(ctx, point); err != nil {
			return err
		}
		fmt.Printf("rewound to #%d\n", point)
		return nil
	case (len(args) == 2 || len(args) == 3) && args[0] == "point":
		// Taken now, with the container frozen; needs time travel on
		// (gcr.dev/time-travel=manual takes points only this way).
		p, err := control.NewClient(ns, args[1]).TakePoint(ctx, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("#%d\n", p.ID)
		return nil
	case len(args) >= 3 && args[0] == "snapshot":
		// The container keeps running, unless -stop; containers created
		// with the annotation gcr.dev/fork-from=DIR start from the
		// snapshot.
		fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
		stop := fs.Bool("stop", false, "stop the container once the snapshot is written: a checkpoint to restore it elsewhere from")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		if *stop {
			return control.NewClient(ns, fs.Arg(0)).SnapshotAndStop(ctx, fs.Arg(1))
		}
		return control.NewClient(ns, fs.Arg(0)).Snapshot(ctx, fs.Arg(1))
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
	case len(args) == 2 && args[0] == "info":
		// What a migration's target must match, as JSON.
		info, err := control.NewClient(ns, args[1]).Info(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(info)
	case len(args) >= 2 && args[0] == "snapshot-rm":
		// Refused while anything uses it.
		var errs []error
		for _, dir := range args[1:] {
			errs = append(errs, snapshot.Remove(dir))
		}
		return errors.Join(errs...)
	case len(args) == 2 && args[0] == snapimage.HolderCommand:
		return snapimage.Holder(args[1])
	case len(args) >= 1 && args[0] == "snapshot-build":
		// Runs IMAGE as a Kubernetes pod's container would run, until it
		// is ready, and pushes a snapshot of it to REF as an image pods
		// run (snapshot-image): for CI, on a host with containerd and gcr.
		return snapshotBuild(ctx, args[1:])
	case len(args) >= 1 && args[0] == "snapshot-image":
		// A snapshot of a running container, pushed to REF as an image
		// that runs as any other, restoring the container's processes
		// (internal/snapimage). The container keeps running.
		fs := flag.NewFlagSet(args[0], flag.ExitOnError)
		plainHTTP := fs.Bool("plain-http", false, "speak HTTP, not HTTPS, to the registry (always for loopback)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		ref, err := snapshotImage(ctx, ns, fs.Arg(0), fs.Arg(1), *plainHTTP)
		if err != nil {
			return err
		}
		fmt.Println(ref)
		return nil
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
	case len(args) >= 1 && args[0] == "shutdown-guard":
		// Run by systemd on a Kubernetes node: pods that opted in to
		// moving (gcr.dev/migrate-on-evict) are checkpointed as the node
		// shuts down, and pushed to REPOSITORY, tagged with their UIDs,
		// for gcr-operator to restore elsewhere.
		fs := flag.NewFlagSet("shutdown-guard", flag.ExitOnError)
		plainHTTP := fs.Bool("plain-http", false, "speak HTTP, not HTTPS, to the registry")
		wait := fs.Duration("wait", 80*time.Second, "how long a shutdown is held at most; logind's InhibitDelayMaxSec must be as long")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 || strings.Contains(path.Base(fs.Arg(0)), ":") || strings.Contains(fs.Arg(0), "@") {
			flag.Usage()
			os.Exit(2)
		}
		if os.Getenv("HOME") == "" {
			// systemd sets none: the registry's credentials are root's
			// Docker config's.
			_ = os.Setenv("HOME", "/root")
		}
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		return nodeshutdown.Guard(ctx, nodeshutdown.GuardOptions{
			Wait: *wait,
			Log:  func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
			Push: func(ctx context.Context, uid string) error {
				repo, err := registry.Repository(fs.Arg(0)+":"+uid, *plainHTTP)
				if err != nil {
					return err
				}
				_, err = registry.Push(ctx, nodeshutdown.CheckpointPath(uid), repo, uid, registry.FormatTar)
				return err
			},
		})
	case len(args) >= 1 && args[0] == "shutdown-plan":
		// gcr-operator's plan for pods of a node shutting down, which it
		// runs there: migrate them live, or restore them from checkpoints.
		fs := flag.NewFlagSet("shutdown-plan", flag.ExitOnError)
		live := fs.Bool("live", false, "the operator migrates them live: their containers run on until it has")
		checkpoint := fs.Bool("checkpoint", false, "they are checkpointed as the kubelet stops them, for the operator to restore")
		released := fs.Bool("released", false, "migrated live, they are handed over to their controllers: the kubelet may stop them")
		_ = fs.Parse(args[1:])
		n := 0
		for _, b := range []bool{*live, *checkpoint, *released} {
			if b {
				n++
			}
		}
		if n != 1 || fs.NArg() == 0 {
			flag.Usage()
			os.Exit(2)
		}
		plan := nodeshutdown.Checkpoint
		switch {
		case *live:
			plan = nodeshutdown.Live
		case *released:
			plan = nodeshutdown.Released
		}
		for _, uid := range fs.Args() {
			if err := nodeshutdown.Plan(uid, plan); err != nil {
				return err
			}
		}
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
		for _, p := range res.Phases {
			fmt.Printf("  %-15s %s\n", p.Name, p.Took.Round(time.Millisecond))
		}
		if res.LazyRestore {
			fmt.Println("the target loads its memory from the images it received, as it runs")
		}
		return nil
	default:
		flag.Usage()
		os.Exit(2)
		return nil
	}
}

// doctor prints what the node has for gcr, and how to get what it lacks.
// It fails if gcr cannot work as it should here.
func doctor() error {
	missing := 0
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, c := range setup.Doctor() {
		mark := map[setup.Level]string{setup.OK: "ok", setup.Optional: "off", setup.Missing: "MISSING"}[c.Level]
		fmt.Fprintf(w, "%s\t%s\t%s\n", mark, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "\t\t-> %s\n", c.Fix)
		}
		if c.Level == setup.Missing {
			missing++
		}
	}
	_ = w.Flush()
	if missing > 0 {
		return fmt.Errorf("%d missing", missing)
	}
	return nil
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

// snapshotRoot is where gcr snapshot-image takes its snapshots, removed
// once pushed.
const snapshotRoot = "/var/lib/gcr/snapshots"

// snapshotImage snapshots container id and pushes it to ref as an image
// (snapimage), returning the image's reference by digest.
func snapshotImage(ctx context.Context, ns, id, ref string, plainHTTP bool) (string, error) {
	c := control.NewClient(ns, id)
	info, err := c.Info(ctx)
	if err != nil {
		return "", err
	}
	if info.IDBase != 0 {
		return "", fmt.Errorf("%s runs in gcr's user namespace (gcr.dev/host-users), which an image made of "+
			"a snapshot cannot carry yet: %w", id, errdefs.ErrNotImplemented)
	}
	if info.Containerd == "" {
		return "", fmt.Errorf("%s's shim does not say where containerd is: it predates gcr snapshot-image", id)
	}
	repo, err := registry.Repository(ref, plainHTTP)
	if err != nil {
		return "", err
	}
	tag := repo.Reference.Reference
	if tag == "" {
		tag = "latest"
	}
	base, closeBase, err := snapimage.FromContainerd(ctx, info.Containerd, ns, id)
	if err != nil {
		return "", err
	}
	defer closeBase()

	dir := filepath.Join(snapshotRoot, fmt.Sprintf(".image-%s-%d", id, time.Now().UnixNano()))
	if err := c.Snapshot(ctx, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	defer func() {
		if err := snapshot.Remove(dir); err != nil {
			fmt.Fprintf(os.Stderr, "gcr: %v (remove it with gcr snapshot-rm)\n", err)
		}
	}()
	desc, err := snapimage.Push(ctx, dir, base, repo, tag)
	if err != nil {
		return "", err
	}
	return repo.Reference.Registry + "/" + repo.Reference.Repository + "@" + desc.Digest.String(), nil
}

// stringsFlag is a flag given any number of times.
type stringsFlag []string

func (f *stringsFlag) String() string     { return strings.Join(*f, ",") }
func (f *stringsFlag) Set(v string) error { *f = append(*f, v); return nil }

func snapshotBuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshot-build", flag.ExitOnError)
	o := snapimage.BuildOptions{ServiceAccount: true}
	var env, volumes, devices stringsFlag
	fs.StringVar(&o.Address, "address", "/run/containerd/containerd.sock", "containerd's socket")
	fs.StringVar(&o.Namespace, "n", "gcr-build", "the containerd namespace to run it in")
	ready := fs.String("ready", "", "a shell command run in the container until it exits 0; the snapshot is taken then")
	fs.DurationVar(&o.Timeout, "timeout", 10*time.Minute, "how long -ready may take")
	fs.DurationVar(&o.Settle, "wait", 0, "how long to wait before the snapshot (after -ready)")
	fs.Var(&env, "env", "KEY=VALUE added to the image's environment (repeatable)")
	fs.Var(&volumes, "volume", "a path the pod mounts a volume at (repeatable): the snapshot must be taken with the pod's mounts")
	fs.Var(&devices, "device", "a CDI device the pod is given (nvidia.com/gpu=0), repeatable: a snapshot of a GPU container must be taken with the GPU")
	noSA := fs.Bool("no-service-account", false, "the pod sets automountServiceAccountToken: false")
	user := fs.String("user", "", "UID[:GID] to run as (the pod's runAsUser, runAsGroup)")
	fs.BoolVar(&o.NoNewPrivileges, "no-new-privileges", false, "the pod sets allowPrivilegeEscalation: false")
	plainHTTP := fs.Bool("plain-http", false, "speak HTTP to registries")
	_ = fs.Parse(args)
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: gcr snapshot-build [flags] IMAGE REF [-- COMMAND...]")
		fs.PrintDefaults()
		os.Exit(2)
	}
	o.Image, o.Env, o.Volumes, o.Devices, o.ServiceAccount, o.PlainHTTP = fs.Arg(0), env, volumes, devices, !*noSA, *plainHTTP
	if o.Args = fs.Args()[2:]; len(o.Args) > 0 && o.Args[0] == "--" {
		o.Args = o.Args[1:]
	}
	if *ready != "" {
		o.Ready = []string{"/bin/sh", "-c", *ready}
	}
	o.UID, o.GID = -1, 0
	if *user != "" {
		u, g, _ := strings.Cut(*user, ":")
		var err error
		if o.UID, err = strconv.Atoi(u); err != nil {
			return fmt.Errorf("-user %q: %w", *user, err)
		}
		if g != "" {
			if o.GID, err = strconv.Atoi(g); err != nil {
				return fmt.Errorf("-user %q: %w", *user, err)
			}
		}
	}
	o.ID = fmt.Sprintf("build-%d", time.Now().UnixNano())
	o.Log = os.Stderr
	o.Say = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "gcr: "+format+"\n", args...) }
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stop, err := snapimage.Build(ctx, o)
	if err != nil {
		return err
	}
	defer stop()
	o.Say("snapshotting it into %s", fs.Arg(1))
	ref, err := snapshotImage(ctx, o.Namespace, o.ID, fs.Arg(1), *plainHTTP)
	if err != nil {
		return err
	}
	fmt.Println(ref)
	return nil
}
