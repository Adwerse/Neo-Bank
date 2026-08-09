// Package failover contains no production code — it exists only to hold
// failover_test.go and the cluster-inspection helpers below. It is a
// separate module rather than a _test.go file inside an existing service
// because what it tests is not a service: it is the docker compose
// topology as a whole, and it needs to shell out to `docker compose` to
// do it, which is a capability no service module should acquire.
package failover

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// pgNodes are the compose service names of the three Patroni-managed
// Postgres nodes. Order is not significant — no node is "the primary";
// which one leads is Patroni's business and changes at runtime, which is
// the entire premise of this package.
var pgNodes = []string{"pg-node1", "pg-node2", "pg-node3"}

// composeProjectDir is where docker-compose.yml lives, relative to this
// package. Every docker command below runs with this as the project
// directory so the test can be invoked from anywhere.
const composeProjectDir = "../.."

// member is one row of Patroni's GET /cluster response.
type member struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	State    string `json:"state"`
	Timeline int    `json:"timeline"`
	Lag      any    `json:"lag"`
}

type clusterView struct {
	Members []member `json:"members"`
}

// isLeader normalises across Patroni versions: 3.x and earlier reported
// the leader's role as "master", 4.x renamed it to "leader". Accepting
// both means this test does not silently start passing vacuously if the
// image's pinned Patroni version moves.
func (m member) isLeader() bool {
	return m.Role == "leader" || m.Role == "master" || m.Role == "standby_leader"
}

// runDocker executes a docker command and returns its stdout. Errors
// include stderr, because docker's failures are almost always explained
// there and almost never in the exit code.
func runDocker(args ...string) (string, error) {
	full := append([]string{"compose", "--project-directory", composeProjectDir}, args...)
	cmd := exec.Command("docker", full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// clusterState asks Patroni for its own view of the cluster.
//
// It tries every node in turn rather than a fixed one, because the whole
// point of this test is that nodes disappear: asking the node that was
// just killed would report an infrastructure failure as a test failure.
// Any surviving node's answer is equally authoritative — they all read
// the same keys out of etcd.
func clusterState(t *testing.T) clusterView {
	t.Helper()

	var lastErr error
	for _, node := range pgNodes {
		out, err := runDocker("exec", "-T", node, "curl", "-fsS", "http://127.0.0.1:8008/cluster")
		if err != nil {
			lastErr = err
			continue
		}
		var view clusterView
		if err := json.Unmarshal([]byte(out), &view); err != nil {
			lastErr = fmt.Errorf("parse /cluster from %s: %w (body: %s)", node, err, out)
			continue
		}
		if len(view.Members) == 0 {
			lastErr = fmt.Errorf("%s reported an empty cluster", node)
			continue
		}
		return view
	}

	t.Fatalf("could not read cluster state from any of %v: %v", pgNodes, lastErr)
	return clusterView{}
}

// leaderName returns the name of the single current leader, failing the
// test if there is not exactly one.
//
// "Exactly one" is not defensive coding here, it is the assertion this
// whole sprint exists to support: two leaders is split-brain, and for a
// ledger that means two divergent histories of the same money. Zero
// leaders during a failover is expected and transient, which is why
// callers use waitForLeader rather than this directly at that moment.
func leaderName(t *testing.T) (string, bool) {
	t.Helper()

	var leaders []string
	for _, m := range clusterState(t).Members {
		if m.isLeader() {
			leaders = append(leaders, m.Name)
		}
	}
	switch len(leaders) {
	case 0:
		return "", false
	case 1:
		return leaders[0], true
	default:
		t.Fatalf("SPLIT BRAIN: %d nodes claim to be leader: %v", len(leaders), leaders)
		return "", false
	}
}

// waitForLeader blocks until exactly one leader exists whose name is not
// excluded, and returns it. Passing the old leader as `excluding` is how
// the test distinguishes "a new leader was elected" from "the old leader
// key has not expired yet".
func waitForLeader(t *testing.T, excluding string, timeout time.Duration) (string, time.Duration) {
	t.Helper()

	start := time.Now()
	deadline := start.Add(timeout)
	for {
		if name, ok := leaderName(t); ok && name != excluding {
			return name, time.Since(start)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new leader elected within %s (excluding %q); cluster: %s", timeout, excluding, clusterSummary(t))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitForMemberRole blocks until `name` appears in the cluster with a
// role satisfying `ok`. Used to assert that a restarted old leader
// rejoins as a follower rather than as a second leader.
func waitForMemberRole(t *testing.T, name string, timeout time.Duration, ok func(member) bool) member {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		for _, m := range clusterState(t).Members {
			if m.Name == name && ok(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach the expected role within %s; cluster: %s", name, timeout, clusterSummary(t))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// clusterSummary renders the cluster compactly for failure messages —
// every assertion failure in this package is easier to diagnose with the
// roles in front of you.
func clusterSummary(t *testing.T) string {
	t.Helper()

	var parts []string
	for _, m := range clusterState(t).Members {
		parts = append(parts, fmt.Sprintf("%s=%s/%s(tl=%d)", m.Name, m.Role, m.State, m.Timeline))
	}
	return strings.Join(parts, " ")
}

// containerIdentity captures enough to prove a container was not
// restarted: the start timestamp and the restart counter. Both are
// needed — a crash-and-restart bumps RestartCount, while a
// `docker compose up` replacement changes StartedAt without necessarily
// touching the counter.
type containerIdentity struct {
	StartedAt    string
	RestartCount string
}

func inspectContainer(t *testing.T, service string) containerIdentity {
	t.Helper()

	out, err := runDocker("ps", "-q", service)
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("could not find a running container for %q: %v", service, err)
	}
	id := strings.Fields(strings.TrimSpace(out))[0]

	cmd := exec.Command("docker", "inspect", "-f", "{{.State.StartedAt}}|{{.RestartCount}}", id)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v: %s", service, err, strings.TrimSpace(stderr.String()))
	}
	fields := strings.SplitN(strings.TrimSpace(string(raw)), "|", 2)
	if len(fields) != 2 {
		t.Fatalf("unexpected docker inspect output for %s: %q", service, string(raw))
	}
	return containerIdentity{StartedAt: fields[0], RestartCount: fields[1]}
}
