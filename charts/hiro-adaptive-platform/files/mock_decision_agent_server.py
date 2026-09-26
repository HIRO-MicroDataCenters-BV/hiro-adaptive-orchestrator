#!/usr/bin/env python3
# charts/hiro-adaptive-platform/files/mock_decision_agent_server.py
#
# COPY of the server.py embedded in hack/mock_decision_agent.yaml's
# ConfigMap (extracted here because it's YAML-embedded there, not a
# standalone file — .Files.Get needs one). Keep in sync by hand if that one
# changes — except the listen port below, which intentionally diverges:
# REPLACE_LISTEN_PORT is substituted with mockAgent.port by
# templates/mock-agent/configmap.yaml at render time (the hack/ original
# has no such templating and keeps its port hardcoded to 8080).
import json
import random
from http.server import HTTPServer, BaseHTTPRequestHandler

PLACEMENT_PATH = "/api/v1/placement/score"
HEALTH_PATH    = "/healthz"

# Placement scoring range — every candidate node gets a random score in
# [PLACEMENT_SCORE_MIN, PLACEMENT_SCORE_MAX].
PLACEMENT_SCORE_MIN = 0.0
PLACEMENT_SCORE_MAX = 100.0

# Rebalance (Move / AdjustReplicas / AdjustResources / Escalate / NoOp)
# knobs. All *_PROBABILITY knobs default to 0.0 — keep NoOp dominant by
# default, bump one of them (see hack/demo_rebalance.sh's
# bump_mock_agent / _scale / _resources / _escalate) when a test
# specifically wants to force that action.
MOVE_PROBABILITY   = 0.0
SCALE_PROBABILITY  = 0.0
RESOURCE_PROBABILITY = 0.0
ESCALATE_PROBABILITY = 0.0
IMPROVEMENT_MIN    = 10.0
IMPROVEMENT_MAX    = 100.0

# Fixed AdjustResources target — deterministic on purpose (unlike Scale's
# random up/down), since this mock only needs one clearly-in-bounds,
# clearly-different-from-current value to demo the enactor, not
# realistic sizing logic. Comfortably inside the operator's default
# guardrail bounds ([50m, 2] CPU, [64Mi, 2Gi] Memory) and above the demo
# app's own starting values (see config/samples/nginx_deployment_2.yaml).
RESOURCE_TARGET_CPU    = "150m"
RESOURCE_TARGET_MEMORY = "192Mi"

class Handler(BaseHTTPRequestHandler):

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw    = self.rfile.read(length) if length else b"{}"

        try:
            req = json.loads(raw)
        except json.JSONDecodeError:
            req = {}

        request_id = req.get("requestId", "mock-id")
        is_rebalance = req.get("rebalanceContext") is not None
        kind = "rebalance" if is_rebalance else "initial-placement"

        self.log_message(
            "POST %s  kind=%s  requestId=%s  request=%s",
            self.path, kind, request_id, json.dumps(req),
        )

        if is_rebalance:
            resp = self._rebalance_response(request_id, req)
        else:
            resp = self._placement_response(request_id, req)

        body = json.dumps(resp).encode()
        self._reply(200, "application/json", body)
        self.log_message(
            "POST %s  kind=%s  requestId=%s  response=%s",
            self.path, kind, request_id, json.dumps(resp),
        )

    def _placement_response(self, request_id, req):
        # CandidateNodes is a list of full Kubernetes Node objects.
        # Extract the name from metadata.name.
        candidate_nodes = req.get("candidateNodes", [])
        node_scores = [
            {
                "nodeName": node.get("metadata", {}).get("name", f"node-{i}"),
                "score": round(random.uniform(PLACEMENT_SCORE_MIN, PLACEMENT_SCORE_MAX), 2),
            }
            for i, node in enumerate(candidate_nodes)
        ]

        # Fallback when the request carries no nodes (shouldn't happen in production, but just in case)
        if not node_scores:
            node_scores = [{
                "nodeName": "fallback-node",
                "score": round(random.uniform(PLACEMENT_SCORE_MIN, PLACEMENT_SCORE_MAX), 2),
            }]

        return {
            "requestId": request_id,
            "nodeScores": node_scores,
            "reason": f"mock: candidate nodes scored randomly in [{PLACEMENT_SCORE_MIN}, {PLACEMENT_SCORE_MAX}]",
        }

    def _rebalance_response(self, request_id, req):
        rebalance_context = req.get("rebalanceContext") or {}
        current_placements = rebalance_context.get("currentPlacements", [])
        candidate_nodes = req.get("candidateNodes", [])

        # candidateNodes for a rebalance decision is every node in the
        # cluster, not just Filter-passed ones (unlike initial-placement
        # scoring) — so control-plane (and other NoSchedule/NoExecute
        # tainted) nodes are still in the list. Recommending one of those
        # as a Move target would always fail once the replacement pod
        # actually tries to schedule there, so skip them the same way a
        # real agent would need to.
        def schedulable(node):
            for taint in node.get("spec", {}).get("taints", []) or []:
                if taint.get("effect") in ("NoSchedule", "NoExecute"):
                    return False
            return True

        candidate_names = [
            n.get("metadata", {}).get("name")
            for n in candidate_nodes
            if n.get("metadata", {}).get("name") and schedulable(n)
        ]

        # Escalate: checked first and unconditionally (no Improvement
        # field — dispatchEscalate, like dispatchNoOp/Reject/Defer,
        # never applies the improvement-threshold guardrail) so bumping
        # this knob alone is enough to force it regardless of what
        # movable/resizable pods happen to be around.
        if random.random() < ESCALATE_PROBABILITY:
            return {
                "requestId": request_id,
                "action": "Escalate",
                "reason": "mock: repeated conflicting signals, needs a human to look",
            }

        movable = [p for p in current_placements if p.get("phase") == "Running"]

        # Only recommend Move when there's a running pod and a candidate
        # node it isn't already on — otherwise fall back to NoOp, same as
        # a real agent would when there's nothing useful to do.
        move_targets = None
        if movable and candidate_names:
            pod = random.choice(movable)
            others = [n for n in candidate_names if n != pod.get("nodeName")]
            if others:
                move_targets = (pod, random.choice(others))

        if move_targets and random.random() < MOVE_PROBABILITY:
            pod, target = move_targets
            return {
                "requestId": request_id,
                "action": "Move",
                "podName": pod.get("podName", ""),
                "targetNode": target,
                "improvement": round(random.uniform(IMPROVEMENT_MIN, IMPROVEMENT_MAX), 2),
                "reason": f"mock: moving {pod.get('podName', '')} to {target} improves balance",
            }

        # AdjustReplicas: current replica count is just how many pods
        # this workload has right now (currentPlacements is every pod of
        # the application). Scale can go either way — up when demand
        # looks high, down when it looks low — so pick a direction at
        # random rather than only ever growing. Never recommend 0 (that's
        # not "adjust", that's "stop"), and never recommend the same
        # count as now — that's a NoOp with extra ceremony, not a real
        # scale.
        current_count = len(current_placements)
        if current_count > 0 and random.random() < SCALE_PROBABILITY:
            if current_count <= 1:
                target_replicas = current_count + 1  # can't scale down below 1
            else:
                target_replicas = current_count + random.choice([-1, 1])
            return {
                "requestId": request_id,
                "action": "AdjustReplicas",
                "targetReplicas": target_replicas,
                "improvement": round(random.uniform(IMPROVEMENT_MIN, IMPROVEMENT_MAX), 2),
                "reason": f"mock: {'scaling up' if target_replicas > current_count else 'scaling down'} from {current_count} to {target_replicas}",
            }

        # AdjustResources: only a container the operator's own guardrail
        # would actually act on — one that already has some CPU/Memory
        # (requests or limits) — is eligible (see
        # internal/rebalance/README.md's "AdjustResources" section for
        # why a container with neither is deferred, not resized).
        resizable = [
            (p, c)
            for p in current_placements
            for c in p.get("containers", [])
            if c.get("cpuRequest") or c.get("memoryRequest") or c.get("cpuLimit") or c.get("memoryLimit")
        ]
        if resizable and random.random() < RESOURCE_PROBABILITY:
            pod, container = random.choice(resizable)
            return {
                "requestId": request_id,
                "action": "AdjustResources",
                "podName": pod.get("podName", ""),
                "containerName": container.get("name", ""),
                "targetCpu": RESOURCE_TARGET_CPU,
                "targetMemory": RESOURCE_TARGET_MEMORY,
                "improvement": round(random.uniform(IMPROVEMENT_MIN, IMPROVEMENT_MAX), 2),
                "reason": f"mock: resizing {container.get('name', '')} to cpu={RESOURCE_TARGET_CPU} memory={RESOURCE_TARGET_MEMORY}",
            }

        return {
            "requestId": request_id,
            "action": "NoOp",
            "improvement": 0,
            "reason": "mock: current placement already optimized",
        }

    def do_GET(self):
        self._reply(200, "text/plain", b"ok")

    def _reply(self, code, content_type, body):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        import sys
        print(f"[mock-decision-agent] {fmt % args}", flush=True, file=sys.stdout)

if __name__ == "__main__":
    addr = ("0.0.0.0", REPLACE_LISTEN_PORT)
    print(f"[mock-decision-agent] listening on {addr[0]}:{addr[1]}", flush=True)
    HTTPServer(addr, Handler).serve_forever()
