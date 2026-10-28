//go:build linux

// gcr is the command line for gcr runtime features outside containerd's API.
//
//	gcr [-n namespace] timeline ID
//	gcr [-n namespace] rewind ID POINT
//	gcr [-n namespace] migrate [-token TOKEN] ID TARGET-URL
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/makhov/gcr/internal/control"
)

func main() {
	ns := flag.String("n", "default", "containerd namespace")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gcr [-n namespace] timeline ID\n"+
			"       gcr [-n namespace] rewind ID POINT\n"+
			"       gcr [-n namespace] migrate [-token TOKEN] ID TARGET-URL\n")
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
	case len(args) >= 1 && args[0] == "migrate":
		fs := flag.NewFlagSet("migrate", flag.ExitOnError)
		token := fs.String("token", os.Getenv("GCR_MIGRATE_TOKEN"), "token the target expects (default $GCR_MIGRATE_TOKEN)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		res, err := control.NewClient(ns, fs.Arg(0)).Migrate(ctx, fs.Arg(1), *token)
		if err != nil {
			return err
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
