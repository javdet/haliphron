package backend

import (
	"context"
	"net/http"
	"strings"
	"testing"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/store"
)

// Deleting a cluster, as distinct from revoking it.
//
// Revocation ends a cluster's ability to act and keeps its row; deletion
// removes the row. The rules checked here are the ones that keep deletion from
// losing anything: only a revoked cluster is deleted, never one that a run
// records as where it ran, and the deletion is audited because nothing else
// will remember the cluster.

func clusterPath(id runv1.ULID) string { return "/api/v1/clusters/" + string(id) }

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) listsCluster(t *testing.T, id runv1.ULID) bool {
	t.Helper()
	clusters, err := h.Store.ListClusters(context.Background())
	if err != nil {
		t.Fatalf("list clusters: %v", err)
	}
	for _, c := range clusters {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (h *harness) revoke(t *testing.T, id runv1.ULID) {
	t.Helper()
	if err := h.Store.RevokeCluster(context.Background(), id, "decommissioned"); err != nil {
		t.Fatalf("revoke %s: %v", id, err)
	}
}

func TestARevokedClusterWithNoRunsCanBeDeletedAndTheDeletionIsAudited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.Token(store.ScopeAdmin)
	victim := newProbe(t, h, "west")
	bystander := newProbe(t, h, "east")
	h.revoke(t, victim.clusterID)

	if status := h.call(t, admin, http.MethodDelete, clusterPath(victim.clusterID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete a revoked cluster: %d", status)
	}
	if h.listsCluster(t, victim.clusterID) {
		t.Fatal("a deleted cluster is still listed")
	}
	if !h.listsCluster(t, bystander.clusterID) {
		t.Fatal("deleting one cluster removed another")
	}

	var action, actor, name, reason string
	if err := h.Store.DB().QueryRowContext(ctx, `
		SELECT action, actor, payload->>'name', payload->>'revokedReason' FROM audit_log
		WHERE subject_kind = 'cluster' AND subject_id = $1 AND action = $2`,
		string(victim.clusterID), store.AuditClusterDeleted).Scan(&action, &actor, &name, &reason); err != nil {
		t.Fatalf("read the audit record of the deletion: %v", err)
	}
	if name != "west" || reason != "decommissioned" || actor != "token:test" {
		t.Fatalf("audit record by %q names %q, revoked for %q", actor, name, reason)
	}

	if status := h.call(t, admin, http.MethodDelete, clusterPath(victim.clusterID), nil, nil); status != http.StatusNotFound {
		t.Fatalf("deleting it twice: %d, want 404", status)
	}
}

// Revoke first, then delete. A cluster that vanished while its controller could
// still act would leave no row saying it was ever ended.
func TestALiveClusterIsNotDeletedUntilItIsRevoked(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	live := newProbe(t, h, "east")

	var body errorBody
	if status := h.call(t, admin, http.MethodDelete, clusterPath(live.clusterID), nil, &body); status != http.StatusConflict {
		t.Fatalf("delete a live cluster: %d, want 409", status)
	}
	if body.Error.Code != "cluster_live" {
		t.Errorf("code = %q, want cluster_live", body.Error.Code)
	}
	if !h.listsCluster(t, live.clusterID) {
		t.Fatal("a refused deletion removed the cluster")
	}
	if _, status := live.Lease(1, 1); status != http.StatusNoContent {
		t.Fatalf("a refused deletion left the cluster unable to lease: %d", status)
	}
}

// The foreign keys from runs and run_attempts are RESTRICT: where a run ran is
// not a thing an operator loses with one DELETE. Once the run is gone, the
// cluster can go too.
func TestAClusterARunRanOnIsKeptUntilTheRunIsDeleted(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	p := newProbe(t, h, "east")
	admitted := h.Submit()

	lease := p.LeaseOne()
	if _, problem := p.Ack(lease.RunID, lease.Epoch); problem != nil {
		t.Fatalf("ack: %+v", problem)
	}
	if _, problem := p.Phase(lease.RunID, lease.Epoch, 1, runv1.PhaseSucceeded); problem != nil {
		t.Fatalf("report succeeded: %+v", problem)
	}
	_, problem := p.Complete(lease.RunID, lease.Epoch, 1, completionFor(lease.RunID, 1, "0.100000", ""))
	fatalIfProblem(t, "completion", problem)
	if status := h.Run(admitted.ID).Status; status != clusterv1.StatusSucceeded {
		t.Fatalf("run status = %s, want Succeeded", status)
	}
	h.revoke(t, p.clusterID)

	var body errorBody
	if status := h.call(t, admin, http.MethodDelete, clusterPath(p.clusterID), nil, &body); status != http.StatusConflict {
		t.Fatalf("delete a cluster a run ran on: %d, want 409", status)
	}
	if body.Error.Code != "cluster_in_use" || !strings.HasPrefix(body.Error.Message, "1 run ") {
		t.Errorf("answer = %s %q, want cluster_in_use naming 1 run", body.Error.Code, body.Error.Message)
	}
	if !h.listsCluster(t, p.clusterID) {
		t.Fatal("a refused deletion removed the cluster")
	}

	if status := h.call(t, admin, http.MethodDelete, deletePath(admitted.ID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete the run: %d", status)
	}
	if status := h.call(t, admin, http.MethodDelete, clusterPath(p.clusterID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete the cluster once its run is gone: %d", status)
	}
}

// With the row gone the key is unknown, and the answer to an unknown key is to
// register again. That is why the bootstrap token's remaining uses matter to
// an operator deleting a cluster whose controller is still installed.
func TestTheControllerOfADeletedClusterIsToldToRegisterAgain(t *testing.T) {
	h := newHarness(t)
	admin := h.Token(store.ScopeAdmin)
	p := newProbe(t, h, "east")
	h.revoke(t, p.clusterID)

	if status := h.call(t, admin, http.MethodDelete, clusterPath(p.clusterID), nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}

	_, problem := p.post("/clusters/"+string(p.clusterID)+"/heartbeat", p.token(),
		clusterv1.HeartbeatRequest{FreeSlots: 1}, nil)
	if problem == nil {
		t.Fatal("a deleted cluster was still served")
	}
	if problem.Action != clusterv1.ActionReregister {
		t.Errorf("action = %s, want reregister", problem.Action)
	}
}

func TestDeletingAClusterNeedsAdmin(t *testing.T) {
	h := newHarness(t)
	p := newProbe(t, h, "east")
	h.revoke(t, p.clusterID)

	reader := h.Token(store.ScopeRunsRead, store.ScopeRunsWrite)
	if status := h.call(t, reader, http.MethodDelete, clusterPath(p.clusterID), nil, nil); status != http.StatusForbidden {
		t.Fatalf("delete without admin: %d, want 403", status)
	}
	if !h.listsCluster(t, p.clusterID) {
		t.Fatal("a forbidden deletion removed the cluster")
	}
}
