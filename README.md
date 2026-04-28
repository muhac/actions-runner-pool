# 🪉 gharp — GitHub Actions Runner Pool

A self-hosted, Docker-based pool of ephemeral GitHub Actions runners.

## ✨ Features

* 🔐 **Self-hosted** — no external service dependency
* ♻️ **Ephemeral runners** — one job per runner, clean environment every time
* ⚡ **Autoscaling** — runners are created on-demand from webhook events
* 📦 **Multi-repository, personal-account support** — share compute across repos (not supported natively by GitHub)

## 🚀 Quick Start

### 1. Run gharp

Pre-built multi-arch image: [`muhac/gharp`](https://hub.docker.com/r/muhac/gharp).

```bash
docker run -d --name gharp \
  -p 8080:8080 \
  -e BASE_URL=https://gharp.example.com \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v gharp-data:/data \
  -v /tmp/gharp:/tmp/gharp \
  muhac/gharp:latest
```

`BASE_URL` must be a public HTTPS URL GitHub can reach.

> ⚠️ **`BASE_URL` is sticky.** It's baked into the GitHub App's webhook
> and OAuth-callback URLs at `/setup` time. Changing it later won't
> reconfigure the App — gharp will log a `BASE_URL drift` warning at
> startup. To migrate, re-run `/setup` (creating a fresh App) or revert
> `BASE_URL` to the original value.

### 2. Create the GitHub App

Open `${BASE_URL}/setup` and click **Create GitHub App**. gharp drives
the GitHub App Manifest flow and persists the credentials locally.

### 3. Install the App

Pick the repos (or "All repositories") you want runners for and submit.

### 4. Add a workflow

```yaml
jobs:
  build:
    runs-on: [self-hosted]
    steps:
      - uses: actions/checkout@v4
      - run: echo "hello from ephemeral runner"
```

Every `workflow_job` whose `runs-on` set intersects `RUNNER_LABELS`
(default `self-hosted`) will get a fresh runner.

📖 More:

- **[`docs/deploy.md`](docs/deploy.md)** — production deployment
  (compose, Cloudflare Tunnel / ngrok / Tailscale Funnel, volumes,
  upgrades, troubleshooting, from-source build).
- **[`docs/configuration.md`](docs/configuration.md)** — every env
  variable, default, and validation rule.
- **[`docs/architecture.md`](docs/architecture.md)** — design decisions
  and invariants.

## 🤔 Why?

GitHub does **not support "user-level" runners**.

* Runners are scoped to:

  * repository
  * organization
  * enterprise

This makes it hard to:

* share a runner across multiple repositories
* use self-hosted runners efficiently in personal accounts
* keep environments clean between jobs
* scale runners dynamically

## 💡 This project solves that

* Uses **GitHub App + webhook (`workflow_job`)**
* Dynamically creates **ephemeral runners per job**
* Automatically cleans up after execution

👉 Result:

> You get **GitHub-hosted-like behavior on your own machine**

## 🏗️ Architecture

```text
GitHub → webhook → pool server → docker run → runner → job → exit
```

* GitHub sends `workflow_job` events
* Autoscaler receives webhook
* Starts a runner container (`EPHEMERAL=1`)
* Runner executes job
* Container exits and is removed

## ⚙️ GitHub App Setup (What happens under the hood)

This project uses **GitHub App Manifest Flow**.

It will automatically:

* create a GitHub App
* set webhook to:

  ```
  https://your-server/github/webhook
  ```
* request permissions:

  * `administration: write`
* subscribe to:

  * `workflow_job`

## 📌 Important Notes

### 🔁 Runner lifecycle

* Each job gets a **fresh runner**
* Runner is automatically removed after execution

### 🐳 Docker requirement

The pool server needs access to Docker:

```yaml
- /var/run/docker.sock:/var/run/docker.sock
```

### ⚠️ Security

This project runs **untrusted workflow code**.

> Do NOT run on sensitive machines.

Recommended:

* dedicated VM
* cloud instance
* homelab node

## ❓ FAQ

### Can I use one runner for multiple repositories?

Not directly.

GitHub only supports repo/org/enterprise-level runners.

👉 This project works around that using dynamic runner creation.

### Do I need Kubernetes?

No.

👉 This project is designed for **single-machine setups using Docker**.

### Can I use this for organizations?

Yes.

But it's primarily optimized for:

* personal accounts
* small teams

## 🎯 Target Use Cases

* Personal GitHub accounts with multiple repositories
* Homelab / NAS CI setups
* Small teams without Kubernetes
* Developers wanting full control over CI environment

## 📦 Tech Stack

* Go (server)
* Docker (runner execution)
* GitHub App (auth + webhook)

## 📄 License

Apache License 2.0

## ⭐ If this helps you

Give it a star — it helps others discover the project!


## 🙌 Acknowledgements

* [GitHub Actions](https://docs.github.com/en/actions) / [self-hosted runners](https://docs.github.com/en/actions/concepts/runners/self-hosted-runners)
* [Docker Github Actions Runner](https://github.com/myoung34/docker-github-actions-runner)
