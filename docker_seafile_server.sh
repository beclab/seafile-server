#!/bin/bash

echo "DB_HOST= $DB_HOST"
echo "DB_PORT= $DB_PORT"
echo "DB_NAME1= $DB_NAME1"
echo "DB_NAME2= $DB_NAME2"
echo "DB_NAME3= $DB_NAME3"
echo "DB_USER= $DB_USER"
echo "DB_PASSWORD= $DB_PASSWORD"

SQL_FILE1="init_pgdata/ccnet.sql"
SQL_FILE2="init_pgdata/seafile.sql"
SQL_FILE3="init_pgdata/seahub.sql"

export PGPASSWORD="$DB_PASSWORD"

TABLE_NAME1="Group"
TABLE_NAME2="branch"
TABLE_NAME3="abuse_reports_abusereport"

psql -h $DB_HOST -p $DB_PORT -U $DB_USER -lqt | cut -d \| -f 1 | grep -qw "$DB_NAME1"
if [ $? -eq 0 ]; then
    echo "database $DB_NAME1 exists"
else
    echo "database $DB_NAME1 doesn't exist"
fi

psql -h $DB_HOST -p $DB_PORT -U $DB_USER -lqt | cut -d \| -f 1 | grep -qw "$DB_NAME2"
if [ $? -eq 0 ]; then
    echo "database $DB_NAME1 exists"
else
    echo "database $DB_NAME1 doesn't exist"
fi

psql -h $DB_HOST -p $DB_PORT -U $DB_USER -lqt | cut -d \| -f 1 | grep -qw "$DB_NAME3"
if [ $? -eq 0 ]; then
    echo "database $DB_NAME1 exists"
else
    echo "database $DB_NAME1 doesn't exist"
fi

unset PGPASSWORD

# Start seaf-server in background and get PID
seaf-server -c ~/dev/conf -d ~/dev/seafile-data -D all -f -l - &
SEAF_PID=$!

# Health check configuration
PORT=8082        # Adjust to your actual seaf-server port
TIMEOUT=30       # Maximum wait time (seconds)
CHECK_INTERVAL=1 # Check interval (seconds)

# Wait for seaf-server to start with health check
echo "Waiting for seaf-server to start (timeout: ${TIMEOUT}s)..."
START_TIME=$(date +%s)
while [ $(( $(date +%s) - START_TIME )) -lt $TIMEOUT ]; do
    if nc -zv localhost $PORT 2>&1; then
        echo "✅ seaf-server started successfully (port $PORT available)"
        break
    fi
    sleep $CHECK_INTERVAL
done

# Check if startup timed out
if ! nc -zv localhost $PORT &>/dev/null; then
    echo "❌ Error: seaf-server failed to start within ${TIMEOUT}s"
    echo "Current status:"
    systemctl status seaf-server || true
    exit 1
fi

# Execute callback.py after health check
echo "Executing callback.py..."
python3 callback.py

# Keep seaf-server running and handle cleanup
echo "seaf-server running (PID:$SEAF_PID)"
wait $SEAF_PID

# Cleanup trap
trap "kill $SEAF_PID 2>/dev/null" EXIT
