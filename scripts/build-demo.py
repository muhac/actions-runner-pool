#!/usr/bin/env python3
"""Build the GitHub Pages demo from the live dashboard template.

The repository ships exactly one dashboard template at
internal/httpapi/handlers/templates/dashboard.html. This script
applies three minimal patches so that template can run as a
standalone, no-backend demo on GitHub Pages:

  1. Rewrite the absolute /css/ link to a relative path so it resolves
     under the Pages subpath (gharp normally serves CSS from /css/,
     Pages serves from /<repo>/demo/).
  2. Inject a banner identifying this as a demo build (and pointing
     readers back at the source repo).
  3. Inject a fetch() shim that serves canned /stats and /jobs
     responses, plus mutates an in-memory job list when Cancel /
     Retry are clicked so the buttons actually do something visible.

The script is intentionally fail-fast: each anchor must occur exactly
once in the source. If a future template change moves the CSS link
or the script tag, the build breaks loudly in CI rather than silently
shipping a misconfigured demo.

Usage:
    python3 scripts/build-demo.py [output_dir]   # default: ./site
"""

from __future__ import annotations

import shutil
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SRC = REPO / "internal/httpapi/handlers/templates/dashboard.html"
CSS = REPO / "internal/httpapi/handlers/templates/css/pico.green.min.css"
OUT_ROOT = Path(sys.argv[1]) if len(sys.argv) > 1 else REPO / "site"
OUT_DIR = OUT_ROOT / "demo"

# Banner is plain HTML so it doesn't depend on any class from the
# template; styled inline so it can't drift if dashboard CSS evolves.
BANNER = """\
<aside style="background:#fff8c5;border-bottom:1px solid #d4a72c;padding:0.5rem 1rem;text-align:center;font-size:0.85rem;color:#6b4d00;">
  🪉 Live demo — UI only, mock data, no backend.
  <a href="https://github.com/muhac/actions-runner-pool" style="color:inherit;text-decoration:underline;">View source on GitHub</a>
</aside>
"""

# Mock shim is injected before the dashboard's own <script>. It
# monkey-patches window.fetch and seeds an in-memory job list. The
# dashboard's apiFetch() goes through fetch() so it sees the mocks
# transparently — no changes to the dashboard JS needed.
SHIM = r"""<script>
(() => {
  const NOW = Date.now();
  const minutesAgo = (n) => new Date(NOW - n * 60_000).toISOString();
  // Mock data: low, round numbers so readers don't mistake the demo
  // for a snapshot of a real deployment. Repos are pinned to public
  // ones the maintainer doesn't mind appearing in screenshots.
  const STATE = {
    jobs: [
      { id: 101, repo: "muhac/muhac", job_name: "update-readme",
        workflow_name: "Profile", labels: "self-hosted",
        run_id: 100, run_attempt: 1, runner_id: 0, runner_name: "",
        status: "pending", conclusion: "", updated_at: minutesAgo(0.5) },
      { id: 102, repo: "muhac/muhac", job_name: "deploy-pages",
        workflow_name: "Profile", labels: "self-hosted",
        run_id: 100, run_attempt: 1, runner_id: 0, runner_name: "",
        status: "dispatched", conclusion: "", updated_at: minutesAgo(1) },
      { id: 103, repo: "muhac/actions-runner-pool", job_name: "test",
        workflow_name: "CI", labels: "self-hosted",
        run_id: 99, run_attempt: 1,
        runner_id: 401, runner_name: "gharp-99-a1b2c3d4",
        status: "in_progress", conclusion: "", updated_at: minutesAgo(2) },
      { id: 104, repo: "muhac/actions-runner-pool", job_name: "smoke",
        workflow_name: "CI", labels: "self-hosted",
        run_id: 99, run_attempt: 1,
        runner_id: 402, runner_name: "gharp-99-d4e5f6a7",
        status: "in_progress", conclusion: "", updated_at: minutesAgo(3) },
      { id: 105, repo: "muhac/actions-runner-pool", job_name: "integration",
        workflow_name: "CI", labels: "self-hosted",
        run_id: 99, run_attempt: 1,
        runner_id: 403, runner_name: "gharp-99-789abc12",
        status: "in_progress", conclusion: "", updated_at: minutesAgo(4) },
      { id: 110, repo: "muhac/actions-runner-pool", job_name: "test",
        workflow_name: "CI", labels: "self-hosted",
        run_id: 98, run_attempt: 1,
        runner_id: 391, runner_name: "gharp-98-x1y2z3w4",
        status: "completed", conclusion: "success", updated_at: minutesAgo(15) },
      { id: 111, repo: "muhac/muhac", job_name: "deploy-pages",
        workflow_name: "Profile", labels: "self-hosted",
        run_id: 95, run_attempt: 1,
        runner_id: 388, runner_name: "gharp-95-4q5w6e7r",
        status: "completed", conclusion: "failure", updated_at: minutesAgo(22) },
      { id: 112, repo: "muhac/actions-runner-pool", job_name: "publish",
        workflow_name: "Release", labels: "self-hosted",
        run_id: 93, run_attempt: 2,
        runner_id: 379, runner_name: "gharp-93-r7t8y9u0",
        status: "completed", conclusion: "cancelled", updated_at: minutesAgo(40) },
      { id: 113, repo: "muhac/actions-runner-pool", job_name: "lint",
        workflow_name: "CI", labels: "self-hosted",
        run_id: 92, run_attempt: 1,
        runner_id: 374, runner_name: "gharp-92-u1i2o3p4",
        status: "completed", conclusion: "success", updated_at: minutesAgo(60) },
      { id: 114, repo: "muhac/muhac", job_name: "update-readme",
        workflow_name: "Profile", labels: "self-hosted",
        run_id: 91, run_attempt: 1,
        runner_id: 366, runner_name: "gharp-91-a9b8c7d6",
        status: "completed", conclusion: "success", updated_at: minutesAgo(90) },
    ],
    capacity: { max_concurrent_runners: 4 },
    nextId: 200,
  };

  const buildStats = () => {
    const byStatus = { pending: 0, dispatched: 0, in_progress: 0, completed: 0 };
    let active = 0;
    for (const j of STATE.jobs) {
      byStatus[j.status] = (byStatus[j.status] || 0) + 1;
      if (j.status === "dispatched" || j.status === "in_progress") active++;
    }
    return {
      jobs: byStatus,
      capacity: {
        active_runners: active,
        available_slots: Math.max(0, STATE.capacity.max_concurrent_runners - active),
        max_concurrent_runners: STATE.capacity.max_concurrent_runners,
      },
    };
  };

  const filterJobs = (params) => {
    const wantStatuses = params.getAll("status");
    const repo = (params.get("repo") || "").trim();
    const limit = parseInt(params.get("limit") || "50", 10);
    let out = STATE.jobs.slice();
    if (wantStatuses.length > 0) out = out.filter(j => wantStatuses.includes(j.status));
    if (repo) out = out.filter(j => j.repo === repo);
    out.sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    return { jobs: out.slice(0, limit) };
  };

  const cancelJob = (id) => {
    const j = STATE.jobs.find(j => j.id === id);
    if (!j) return false;
    if (j.status !== "pending" && j.status !== "dispatched") return false;
    j.status = "completed";
    j.conclusion = "cancelled";
    j.updated_at = new Date().toISOString();
    return true;
  };

  const retryJob = (id) => {
    const orig = STATE.jobs.find(j => j.id === id);
    if (!orig || orig.status !== "completed") return false;
    STATE.jobs.unshift({
      ...orig,
      id: STATE.nextId++,
      run_attempt: orig.run_attempt + 1,
      status: "pending",
      conclusion: "",
      runner_id: 0,
      runner_name: "",
      updated_at: new Date().toISOString(),
    });
    return true;
  };

  const json = (obj, status = 200) => Promise.resolve(new Response(
    JSON.stringify(obj),
    { status, headers: { "Content-Type": "application/json" } }
  ));
  const text = (body, status = 200) => Promise.resolve(new Response(
    body, { status, headers: { "Content-Type": "text/plain" } }
  ));

  const realFetch = window.fetch.bind(window);
  window.fetch = function (input, init) {
    const url = typeof input === "string" ? input : input.url;
    const u = new URL(url, location.href);
    const method = (init?.method || "GET").toUpperCase();

    if (u.pathname === "/stats" && method === "GET") return json(buildStats());
    if (u.pathname === "/jobs" && method === "GET") return json(filterJobs(u.searchParams));

    const action = u.pathname.match(/^\/jobs\/(\d+)\/(retry|cancel)$/);
    if (action && method === "POST") {
      const id = parseInt(action[1], 10);
      const ok = action[2] === "retry" ? retryJob(id) : cancelJob(id);
      return ok
        ? json({ ok: true })
        : text("not actionable in current state", 409);
    }

    // Anything else (e.g. CSS, favicon) goes through the real fetch.
    return realFetch(input, init);
  };

  // The token affordance is meaningless on a no-auth demo — hide it
  // rather than have it open a useless dialog.
  document.addEventListener("DOMContentLoaded", () => {
    const t = document.getElementById("tokenButton");
    if (t) t.style.display = "none";
  });
})();
</script>
"""


def patch(html: str) -> str:
    """Apply the three string patches; raise on any anchor mismatch."""
    css_anchor = 'href="/css/pico.green.min.css"'
    body_anchor = "<body>"
    script_anchor = "  <script>"

    if html.count(css_anchor) != 1:
        sys.exit(
            f"build-demo: expected exactly 1 CSS anchor "
            f"({css_anchor!r}), found {html.count(css_anchor)}"
        )
    if html.count(body_anchor) != 1:
        sys.exit(
            f"build-demo: expected exactly 1 body open tag, "
            f"found {html.count(body_anchor)}"
        )
    if html.count(script_anchor) != 1:
        sys.exit(
            f"build-demo: expected exactly 1 leading-2-space "
            f"<script> anchor, found {html.count(script_anchor)}"
        )

    html = html.replace(css_anchor, 'href="css/pico.green.min.css"', 1)
    html = html.replace(body_anchor, body_anchor + "\n" + BANNER, 1)
    html = html.replace(script_anchor, SHIM + script_anchor, 1)
    return html


def main() -> None:
    if not SRC.exists():
        sys.exit(f"build-demo: source not found at {SRC}")
    if not CSS.exists():
        sys.exit(f"build-demo: pico CSS not found at {CSS}")

    OUT_DIR.mkdir(parents=True, exist_ok=True)
    (OUT_DIR / "css").mkdir(exist_ok=True)
    shutil.copy(CSS, OUT_DIR / "css/pico.green.min.css")

    out_html = patch(SRC.read_text())
    (OUT_DIR / "index.html").write_text(out_html)

    print(f"wrote {OUT_DIR / 'index.html'} ({len(out_html):,} bytes)")
    print(f"wrote {OUT_DIR / 'css/pico.green.min.css'}")


if __name__ == "__main__":
    main()
