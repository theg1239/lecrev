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

if command -v postgresql-setup >/dev/null 2>&1; then
  if [[ ! -f /var/lib/pgsql/data/PG_VERSION && ! -f /var/lib/pgsql/15/data/PG_VERSION ]]; then
    postgresql-setup --initdb --unit "${pg_unit}" >/dev/null 2>&1 || postgresql-setup --initdb >/dev/null 2>&1
  fi
fi

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

echo "postgres initialized for database ${DB_NAME}"
