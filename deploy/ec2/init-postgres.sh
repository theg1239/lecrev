#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  exec sudo --preserve-env=LECREV_DB_NAME,LECREV_DB_USER,LECREV_DB_PASSWORD "$0" "$@"
fi

DB_NAME="${LECREV_DB_NAME:-lecrev}"
DB_USER="${LECREV_DB_USER:-lecrev}"
DB_PASSWORD="${LECREV_DB_PASSWORD:-}"

if [[ -z "${DB_PASSWORD}" ]]; then
  echo "set LECREV_DB_PASSWORD before running this script" >&2
  exit 1
fi

pg_service="${POSTGRES_SERVICE:-postgresql.service}"
pg_unit="${pg_service%.service}"
pg_data_dir="${POSTGRES_DATA_DIR:-}"

if command -v postgresql-setup >/dev/null 2>&1; then
  if [[ ! -f /var/lib/pgsql/data/PG_VERSION && ! -f /var/lib/pgsql/15/data/PG_VERSION ]]; then
    postgresql-setup --initdb --unit "${pg_unit}" >/dev/null 2>&1 || postgresql-setup --initdb >/dev/null 2>&1
  fi
fi

if [[ -z "${pg_data_dir}" ]]; then
  if [[ -f /var/lib/pgsql/data/PG_VERSION ]]; then
    pg_data_dir="/var/lib/pgsql/data"
  elif [[ -f /var/lib/pgsql/15/data/PG_VERSION ]]; then
    pg_data_dir="/var/lib/pgsql/15/data"
  else
    echo "failed to determine PostgreSQL data directory" >&2
    exit 1
  fi
fi

pg_hba_conf="${POSTGRES_HBA_FILE:-${pg_data_dir}/pg_hba.conf}"

python3 - "${pg_hba_conf}" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
text = path.read_text()
replacements = {
    "host    all             all             127.0.0.1/32            ident": "host    all             all             127.0.0.1/32            scram-sha-256",
    "host    all             all             ::1/128                 ident": "host    all             all             ::1/128                 scram-sha-256",
    "host    replication     all             127.0.0.1/32            ident": "host    replication     all             127.0.0.1/32            scram-sha-256",
    "host    replication     all             ::1/128                 ident": "host    replication     all             ::1/128                 scram-sha-256",
}
for old, new in replacements.items():
    text = text.replace(old, new)
path.write_text(text)
PY

systemctl enable --now "${pg_service}"

escaped_password="$(printf "%s" "${DB_PASSWORD}" | sed "s/'/''/g")"

sudo -u postgres psql -v ON_ERROR_STOP=1 <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${DB_USER}') THEN
    EXECUTE 'CREATE ROLE ${DB_USER} LOGIN PASSWORD ''${escaped_password}''';
  ELSE
    EXECUTE 'ALTER ROLE ${DB_USER} WITH LOGIN PASSWORD ''${escaped_password}''';
  END IF;
END
\$\$;
SQL

db_exists="$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'")"
if [[ "${db_exists}" != "1" ]]; then
  sudo -u postgres createdb --owner="${DB_USER}" "${DB_NAME}"
fi

systemctl reload "${pg_service}" || systemctl restart "${pg_service}"

echo "postgres initialized for database ${DB_NAME}"
