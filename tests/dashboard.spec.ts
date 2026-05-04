import { test, expect, Route } from "@playwright/test";

// ---------------------------------------------------------------------------
// Fixtures / helpers
// ---------------------------------------------------------------------------

const STATS_EMPTY = {
  jobs: { pending: 0, dispatched: 0, in_progress: 0, completed: 0 },
  capacity: { active_runners: 0, available_slots: 4, max_concurrent_runners: 4 },
};

const STATS_BUSY = {
  jobs: { pending: 2, dispatched: 1, in_progress: 3, completed: 17 },
  capacity: { active_runners: 8, available_slots: 2, max_concurrent_runners: 10 },
};

const STATS_FULL = {
  jobs: { pending: 0, dispatched: 0, in_progress: 10, completed: 5 },
  capacity: { active_runners: 10, available_slots: 0, max_concurrent_runners: 10 },
};

const JOBS_EMPTY = { jobs: [] };

const JOBS_TWO = {
  jobs: [
    {
      id: 1,
      repo: "org/repo-a",
      job_name: "build",
      workflow: "CI",
      status: "in_progress",
      runner_name: "runner-abc",
      updated_at: new Date().toISOString(),
    },
    {
      id: 2,
      repo: "org/repo-b",
      job_name: "lint",
      workflow: "Lint",
      status: "pending",
      runner_name: "",
      updated_at: new Date().toISOString(),
    },
  ],
};

const JOBS_WITH_COMPLETED = {
  jobs: [
    {
      id: 1,
      repo: "org/repo-a",
      job_name: "build",
      workflow: "CI",
      status: "completed",
      conclusion: "success",
      runner_name: "runner-abc",
      updated_at: new Date().toISOString(),
    },
  ],
};

async function mockApis(
  route: Route,
  stats = STATS_EMPTY,
  jobs = JOBS_EMPTY
): Promise<void> {
  // Each call handles a single route; callers wire this up with page.route.
  const url = route.request().url();
  if (url.includes("/stats")) {
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(stats) });
  } else if (url.includes("/jobs")) {
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(jobs) });
  } else {
    await route.continue();
  }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test.beforeEach(async ({ page }) => {
  // Clear sessionStorage between tests so token state doesn't leak.
  await page.addInitScript(() => sessionStorage.clear());
});

test("renders page shell with required elements", async ({ page }) => {
  await page.route("**/stats", (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) }));
  await page.route("**/jobs**", (r) => r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) }));

  await page.goto("/");

  await expect(page.locator("#app")).toBeVisible();
  await expect(page.locator("#authForm")).toBeAttached();
  await expect(page.locator(".stats")).toBeVisible();
  await expect(page.locator("thead tr th")).toHaveCount(6);
  await expect(page.locator("#capacityFill")).toBeVisible();
});

test("stat tiles update from /stats response", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_BUSY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  await expect(page.locator("#statPending")).toHaveText("2");
  await expect(page.locator("#statDispatched")).toHaveText("1");
  await expect(page.locator("#statInProgress")).toHaveText("3");
  await expect(page.locator("#statCompleted")).toHaveText("17");
  await expect(page.locator("#statActive")).toHaveText("8");
  await expect(page.locator("#statAvailable")).toHaveText("2");
});

test("renders job rows from /jobs response", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_TWO) })
  );

  await page.goto("/");

  const rows = page.locator("#jobsBody tr");
  await expect(rows).toHaveCount(2);
  await expect(page.locator("#tableCount")).toHaveText("2 jobs");
  await expect(page.locator("#emptyState")).toBeHidden();

  // Verify badge classes exist for the two statuses.
  await expect(rows.nth(0).locator(".badge.in_progress")).toBeVisible();
  await expect(rows.nth(1).locator(".badge.pending")).toBeVisible();
});

test("empty state shown when /jobs returns no results", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  await expect(page.locator("#emptyState")).toBeVisible();
  await expect(page.locator("#jobsBody tr")).toHaveCount(0);
  await expect(page.locator("#tableCount")).toHaveText("0 jobs");
});

test("filtering by status sends correct query params", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  // A checkbox change triggers refresh automatically, so wait for the first
  // request that already includes both selected statuses.
  const jobsRequest = page.waitForRequest((req) => {
    const url = req.url();
    return url.includes("/jobs") && url.includes("status=pending") && url.includes("status=in_progress");
  });

  // Check "pending" checkbox.
  await page.locator('input[name=status][value=pending]').check();
  // Check "in_progress" checkbox.
  await page.locator('input[name=status][value=in_progress]').check();
  await expect(page.locator('input[name=status][value=pending]')).toBeChecked();
  await expect(page.locator('input[name=status][value=in_progress]')).toBeChecked();
  // Click Apply to trigger the request.
  await page.locator("#applyFiltersButton").click();

  const req = await jobsRequest;
  const url = new URL(req.url());
  const statuses = url.searchParams.getAll("status");
  expect(statuses.length).toBeGreaterThan(0);
  expect(statuses).toContain("pending");
  expect(statuses).toContain("in_progress");
});

test("filtering by repo sends correct query params", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  const jobsRequest = page.waitForRequest((req) => req.url().includes("/jobs") && req.url().includes("repo="));

  await page.locator("#repoInput").fill("myorg/myrepo");
  await page.locator("#applyFiltersButton").click();

  const req = await jobsRequest;
  const url = new URL(req.url());
  expect(url.searchParams.get("repo")).toBe("myorg/myrepo");
});

test("token panel toggles on Token button click", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  // Auth panel starts hidden.
  await expect(page.locator("#authPanel")).toBeHidden();

  await page.locator("#tokenButton").click();
  await expect(page.locator("#authPanel")).toBeVisible();
});

test("saves token to sessionStorage on form submit", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  await page.locator("#tokenButton").click();
  await page.locator("#tokenInput").fill("my-secret-token");
  await page.locator("#saveTokenButton").click();

  const stored = await page.evaluate(() =>
    sessionStorage.getItem("gharp.adminToken")
  );
  expect(stored).toBe("my-secret-token");
});

test("clear token button removes token from sessionStorage", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  // Plant a token first via the form.
  await page.locator("#tokenButton").click();
  await page.locator("#tokenInput").fill("to-be-cleared");
  await page.locator("#saveTokenButton").click();

  // Now clear it.
  await page.locator("#tokenButton").click();
  await page.locator("#clearTokenButton").click();

  const stored = await page.evaluate(() =>
    sessionStorage.getItem("gharp.adminToken")
  );
  expect(stored).toBeNull();
});

test("retry button calls POST /jobs/:id/retry", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_WITH_COMPLETED) })
  );

  let retryCalled = false;
  await page.route("**/jobs/1/retry", (r) => {
    retryCalled = true;
    r.fulfill({ status: 200, body: "" });
  });

  await page.goto("/");

  // Wait for the table to populate, then click the Retry button in the first row.
  await expect(page.locator("#jobsBody tr")).not.toHaveCount(0);
  await page.getByRole("button", { name: /retry/i }).first().click();
  expect(retryCalled).toBe(true);
});

test("cancel button calls POST /jobs/:id/cancel", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_TWO) })
  );

  let cancelCalled = false;
  await page.route("**/jobs/2/cancel", (r) => {
    cancelCalled = true;
    r.fulfill({ status: 200, body: "" });
  });

  await page.goto("/");

  // Second row (id=2) – cancel.
  await page.locator("#jobsBody tr").nth(1).locator("button", { hasText: /cancel/i }).click();
  expect(cancelCalled).toBe(true);
});

test("capacity bar shows warn state at 75%+", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_BUSY) }) // 8/10 = 80%
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  await expect(page.locator("#capacityFill")).toHaveAttribute("data-state", "warn");
});

test("capacity bar shows full state at 100%", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_FULL) }) // 10/10 = 100%
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  await expect(page.locator("#capacityFill")).toHaveAttribute("data-state", "full");
});

test("401 from /stats triggers auth panel", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ status: 401, body: "Unauthorized" })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ status: 401, body: "Unauthorized" })
  );

  await page.goto("/");

  await expect(page.locator("#authPanel")).toBeVisible();
});

test("error banner visible on API failure", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ status: 500, body: "internal error" })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  await expect(page.locator("#errorBanner")).toBeVisible();
  await expect(page.locator("#errorBanner")).not.toBeHidden();
});

test("last updated text shown after refresh", async ({ page }) => {
  await page.route("**/stats", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(STATS_EMPTY) })
  );
  await page.route("**/jobs**", (r) =>
    r.fulfill({ contentType: "application/json", body: JSON.stringify(JOBS_EMPTY) })
  );

  await page.goto("/");

  // After initial auto-refresh the text should change from "Not refreshed yet".
  await expect(page.locator("#lastUpdated")).not.toHaveText("Not refreshed yet");
});
