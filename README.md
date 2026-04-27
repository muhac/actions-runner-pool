# 🪉 gharp — GitHub Actions Runner Pool

**Ephemeral autoscaling GitHub Actions runners for multiple repositories and personal accounts (Docker-based, no Kubernetes).**

Run a single self-hosted runner across multiple repositories — even under a personal account — with clean, ephemeral environments for every job.

## ✨ Features

* ♻️ **Ephemeral runners** — one job per runner, clean environment every time
* ⚡ **Autoscaling** — runners are created on-demand from webhook events
* 🧑‍💻 **Personal account support** — no organization required
* 📦 **Multi-repository** — share compute across repos (not supported natively by GitHub)
* 🐳 **Docker-based** — simple, no Kubernetes required
* 🔐 **Self-hosted** — no external service dependency

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

## 🚀 Quick Start

### 1. Clone

```bash
git clone https://github.com/<yourname>/actions-runner-pool
cd actions-runner-pool
```

### 2. Configure

```bash
cp .env.example .env
```

Edit `.env`:

```env
PORT=8080
BASE_URL=https://your-server.example.com
```

### 3. Run

```bash
docker compose up -d
```

### 4. Open setup page

```text
http://localhost:8080/setup
```

👉 Click **"Create GitHub App"**

### 5. Done

* App is created
* Webhook configured
* Credentials stored locally

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

## 🧪 Usage

In your repository:

```yaml
jobs:
  build:
    runs-on: [self-hosted, ephemeral]
    steps:
      - uses: actions/checkout@v4
      - run: echo "hello from ephemeral runner"
```

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
