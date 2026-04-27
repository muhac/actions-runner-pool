CREATE TABLE IF NOT EXISTS app_config (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  app_id          INTEGER NOT NULL,
  slug            TEXT    NOT NULL,
  webhook_secret  TEXT    NOT NULL,
  pem             BLOB    NOT NULL,
  client_id       TEXT    NOT NULL,
  client_secret   TEXT    NOT NULL DEFAULT '',
  base_url        TEXT    NOT NULL,
  created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS installations (
  id             INTEGER PRIMARY KEY,
  account_id     INTEGER NOT NULL,
  account_login  TEXT    NOT NULL,
  account_type   TEXT    NOT NULL,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS installation_repos (
  repo            TEXT    PRIMARY KEY,
  installation_id INTEGER NOT NULL,
  added_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (installation_id) REFERENCES installations(id)
);
CREATE INDEX IF NOT EXISTS idx_installation_repos_inst ON installation_repos(installation_id);

CREATE TABLE IF NOT EXISTS jobs (
  id           INTEGER PRIMARY KEY,
  repo         TEXT    NOT NULL,
  action       TEXT    NOT NULL,
  labels       TEXT    NOT NULL,
  dedupe_key   TEXT    NOT NULL UNIQUE,
  status       TEXT    NOT NULL,
  conclusion   TEXT    NOT NULL DEFAULT '',
  runner_id    INTEGER NOT NULL DEFAULT 0,
  runner_name  TEXT    NOT NULL DEFAULT '',
  received_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

CREATE TABLE IF NOT EXISTS runners (
  container_name TEXT    PRIMARY KEY,
  repo           TEXT    NOT NULL,
  runner_name    TEXT    NOT NULL,
  labels         TEXT    NOT NULL,
  status         TEXT    NOT NULL,
  started_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  finished_at    DATETIME
);
CREATE INDEX IF NOT EXISTS idx_runners_status ON runners(status);
CREATE INDEX IF NOT EXISTS idx_runners_runner_name ON runners(runner_name);
