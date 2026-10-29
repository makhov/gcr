//go:build e2e && linux

// Package workloads forks real programs, not test helpers: an
// interpreter, a JIT runtime and a database, each holding state built
// before the snapshot. Every fork must keep serving on the socket it
// was listening on, find its state intact and go its own way from there,
// and the parent must carry on unaffected. Which programs CRIU can save
// is the question behind "fork works": this is where it gets answered.
package workloads

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/makhov/gcr/internal/container"
	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

const forks = 3

// The servers hold a 48 MiB "model" made at start, its SHA-256 then
// ("loaded") and now ("digest"), and a counter /inc moves: a fork must
// answer with the snapshot's counter and a model that still hashes the
// same. They listen on 127.0.0.1:8000 in the container's own network
// namespace; the client runs in the container (exec).

const pythonServer = `
import hashlib, http.server, json, os, random, signal
# A fork started with gcr.dev/fork-signal=SIGUSR2 gets it once restored.
signal.signal(signal.SIGUSR2, lambda *_: random.seed())
model = bytes(os.urandom(1 << 20)) * 48
loaded = hashlib.sha256(model).hexdigest()
count = 0
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global count
        if self.path == "/inc":
            count += 1
        body = {"count": count, "loaded": loaded, "digest": hashlib.sha256(model).hexdigest()}
        if self.path == "/rand":
            body["rand"] = random.random()
        b = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", 8000), H).serve_forever()
`

const nodeServer = `
const http = require("http"), crypto = require("crypto");
const model = crypto.randomBytes(48 << 20);
const hash = () => crypto.createHash("sha256").update(model).digest("hex");
const loaded = hash();
let count = 0;
http.createServer((req, res) => {
  if (req.url === "/inc") count++;
  res.end(JSON.stringify({count, loaded, digest: hash()}));
}).listen(8000, "127.0.0.1");
`

const javaServer = `
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.security.MessageDigest;
import java.util.HexFormat;
import java.util.Random;

public class Server {
    static int count;
    static final byte[] model = new byte[48 << 20];

    public static void main(String[] args) throws Exception {
        new Random().nextBytes(model);
        String loaded = hash();
        HttpServer s = HttpServer.create(new InetSocketAddress("127.0.0.1", 8000), 0);
        s.createContext("/", x -> {
            String body;
            synchronized (Server.class) {
                if (x.getRequestURI().getPath().equals("/inc")) count++;
                body = String.format("{\"count\":%d,\"loaded\":\"%s\",\"digest\":\"%s\"}", count, loaded, hash());
            }
            byte[] b = body.getBytes();
            x.sendResponseHeaders(200, b.length);
            x.getResponseBody().write(b);
            x.close();
        });
        s.start();
    }

    static String hash() {
        try {
            return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(model));
        } catch (Exception e) {
            throw new RuntimeException(e);
        }
    }
}
`

type server struct {
	name  string
	image string
	args  []string
	// get is the client command for a path, run in the container.
	get func(path string) []string
}

var servers = []server{
	{
		name:  "python",
		image: "docker.io/library/python:3.13-slim",
		args:  []string{"python3", "-c", pythonServer},
		get: func(path string) []string {
			return []string{"python3", "-c", "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8000" + path + "').read().decode())"}
		},
	},
	{
		name:  "node",
		image: "docker.io/library/node:22-slim",
		args:  []string{"node", "-e", nodeServer},
		get: func(path string) []string {
			return []string{"node", "-e", "fetch('http://127.0.0.1:8000" + path + "').then(r => r.text()).then(t => console.log(t))"}
		},
	},
	{
		name:  "java",
		image: "docker.io/library/eclipse-temurin:21-jdk-alpine",
		args:  []string{"sh", "-c", "cat > /tmp/Server.java <<'EOF'\n" + javaServer + "\nEOF\nexec java /tmp/Server.java"},
		get: func(path string) []string {
			return []string{"wget", "-qO-", "http://127.0.0.1:8000" + path}
		},
	},
}

type reply struct {
	Count  int     `json:"count"`
	Loaded string  `json:"loaded"`
	Digest string  `json:"digest"`
	Rand   float64 `json:"rand"`
}

func get(t *testing.T, s server, c *framework.Container, path string) (reply, error) {
	t.Helper()
	out, code := c.Exec(s.get(path)...)
	var r reply
	if code != 0 {
		return r, fmt.Errorf("exit %d: %s", code, out)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil {
		return r, fmt.Errorf("%w: %q", err, out)
	}
	return r, nil
}

// settled waits until no TCP connection in c is established: the client
// just run may still be closing its connection, and CRIU refuses to dump
// one (connections are not restored).
func settled(t *testing.T, c *framework.Container) {
	t.Helper()
	begin := time.Now()
	defer func() { t.Logf("connections closed after %s", time.Since(begin).Round(time.Millisecond)) }()
	framework.Eventually(t, 30*time.Second, "connections to close", func() bool {
		for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			out, code := c.Exec("cat", f)
			if code != 0 {
				continue // no IPv6
			}
			for _, l := range strings.Split(out, "\n")[1:] {
				if fields := strings.Fields(l); len(fields) > 3 && fields[3] == "01" { // ESTABLISHED
					return false
				}
			}
		}
		return true
	})
}

func TestServers(t *testing.T) {
	for _, s := range servers {
		t.Run(s.name, func(t *testing.T) { testServer(t, s) })
	}
}

func testServer(t *testing.T, s server) {
	ns := framework.NewNamespace(t, "gcr-e2e")
	parent := ns.Run(framework.UniqueID(s.name), framework.Spec{Image: s.image, Args: s.args})
	framework.Eventually(t, 3*time.Minute, s.name+" to serve", func() bool {
		_, err := get(t, s, parent, "/")
		return err == nil
	})
	const before = 3
	for range before {
		if _, err := get(t, s, parent, "/inc"); err != nil {
			t.Fatalf("parent: %v", err)
		}
	}
	settled(t, parent)
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	begin := time.Now()
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Logf("snapshot in %s", time.Since(begin).Round(time.Millisecond))

	children := make([]*framework.Container, forks)
	for i := range children {
		begin := time.Now()
		children[i] = ns.Run(framework.UniqueID(fmt.Sprintf("%s-fork%d", s.name, i)), framework.Spec{
			Image: s.image, Args: s.args,
			Annotations: map[string]string{container.AnnotationForkFrom: dir},
		})
		t.Logf("fork %d started in %s", i, time.Since(begin).Round(time.Millisecond))
	}
	// Each fork answers on the socket the parent was listening on, with
	// the parent's state, and moves on alone: one /inc each takes every
	// fork to before+1, not to before+1, +2, +3 as a shared counter would.
	for i, c := range children {
		r, err := get(t, s, c, "/inc")
		if err != nil {
			t.Fatalf("fork %d: %v", i, err)
		}
		if r.Count != before+1 {
			t.Fatalf("fork %d: count %d after one /inc, want %d", i, r.Count, before+1)
		}
		if r.Digest != r.Loaded || r.Loaded == "" {
			t.Fatalf("fork %d: the model hashes to %s, %s when loaded: memory came back wrong", i, r.Digest, r.Loaded)
		}
	}
	r, err := get(t, s, parent, "/inc")
	if err != nil || r.Count != before+1 || r.Digest != r.Loaded {
		t.Fatalf("parent after the forks: %+v, %v", r, err)
	}
	// What gcr.dev/fork-signal and /.gcr/fork.json are for: random state
	// seeded before the snapshot is the same in every fork.
	if s.name == "python" {
		a, errA := get(t, s, children[0], "/rand")
		b, errB := get(t, s, children[1], "/rand")
		if errA != nil || errB != nil || a.Rand != b.Rand {
			t.Fatalf("forks' first random numbers: %v, %v (%v, %v); want the same", a.Rand, b.Rand, errA, errB)
		}
		t.Logf("two forks draw the same random number, %v", a.Rand)

		// A fork that asked for the signal re-seeds in its handler.
		reseeded := ns.Run(framework.UniqueID("python-reseeded"), framework.Spec{
			Image: s.image, Args: s.args,
			Annotations: map[string]string{
				container.AnnotationForkFrom:   dir,
				container.AnnotationForkSignal: "SIGUSR2",
			},
		})
		c, err := get(t, s, reseeded, "/rand")
		if err != nil || c.Rand == a.Rand {
			t.Fatalf("re-seeded fork drew %v (%v), the same as the others", c.Rand, err)
		}
	}
}

// TestPostgres forks a database with a table in it: each fork has the
// rows the snapshot had, takes writes of its own, and none sees
// another's. Postgres is several processes sharing memory, with its
// data in the writable layer, which the forks share underneath their own.
func TestPostgres(t *testing.T) {
	const image = "docker.io/library/postgres:17-alpine"
	spec := framework.Spec{
		Image:    image,
		SpecOpts: []oci.SpecOpts{oci.WithEnv([]string{"POSTGRES_HOST_AUTH_METHOD=trust"})},
	}
	ns := framework.NewNamespace(t, "gcr-e2e")
	sql := func(c *framework.Container, query string) (string, error) {
		// TCP, not the socket: only the server initdb's temporary one is
		// replaced by listens there.
		out, code := c.Exec("psql", "-h", "127.0.0.1", "-U", "postgres", "-tAc", query)
		if code != 0 {
			return "", fmt.Errorf("%s: exit %d: %s", query, code, out)
		}
		return strings.TrimSpace(out), nil
	}
	count := func(t *testing.T, c *framework.Container) int {
		t.Helper()
		out, err := sql(c, "select count(*) from t")
		if err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		n, err := strconv.Atoi(out)
		if err != nil {
			t.Fatalf("%s: count is %q", c.ID, out)
		}
		return n
	}

	parent := ns.Run(framework.UniqueID("postgres"), spec)
	framework.Eventually(t, 3*time.Minute, "postgres to accept connections", func() bool {
		_, err := sql(parent, "select 1")
		return err == nil
	})
	if _, err := sql(parent, "create table t (id serial primary key, v text); insert into t (v) select md5(g::text) from generate_series(1, 1000) g"); err != nil {
		t.Fatal(err)
	}
	settled(t, parent)
	dir := filepath.Join(framework.TempDir(t), "snapshot")
	if err := control.NewClient(ns.Name, parent.ID).Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	children := make([]*framework.Container, forks)
	for i := range children {
		s := spec
		s.Annotations = map[string]string{container.AnnotationForkFrom: dir}
		children[i] = ns.Run(framework.UniqueID(fmt.Sprintf("postgres-fork%d", i)), s)
	}
	for i, c := range children {
		if n := count(t, c); n != 1000 {
			t.Fatalf("fork %d: %d rows, want the snapshot's 1000", i, n)
		}
		// Fork i adds i+1 rows; a shared table would show the sum.
		if _, err := sql(c, fmt.Sprintf("insert into t (v) select 'fork' from generate_series(1, %d)", i+1)); err != nil {
			t.Fatalf("fork %d: %v", i, err)
		}
	}
	for i, c := range children {
		if n := count(t, c); n != 1000+i+1 {
			t.Fatalf("fork %d: %d rows, want %d: forks share a table", i, n, 1000+i+1)
		}
		// A checkpoint after the writes: the database is consistent
		// enough for Postgres to check itself.
		if out, err := sql(c, "checkpoint"); err != nil {
			t.Fatalf("fork %d: checkpoint: %v %s", i, err, out)
		}
	}
	if n := count(t, parent); n != 1000 {
		t.Fatalf("parent: %d rows after the forks wrote, want 1000", n)
	}
}
